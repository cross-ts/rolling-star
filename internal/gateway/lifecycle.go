package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/router"
)

// shutdownTimeout bounds how long handleShutdown waits for all
// downstream servers to answer "shutdown" before giving up on the slow
// ones and replying to the upstream client anyway.
const shutdownTimeout = 5 * time.Second

// exitGracePeriod is how long handleExit waits for a downstream process
// to exit on its own, after sending it "exit" and closing its stdin,
// before force-terminating it.
const exitGracePeriod = 2 * time.Second

// downstreamInitializeTimeout bounds how long handleInitialize waits for
// a single downstream's "initialize" call to complete. Real language
// servers can be genuinely slow to start (indexing, warming caches,
// ...), so this is deliberately generous -- not a responsiveness check.
// Its actual job is to turn a downstream that never replies (whether
// hung, crashed without closing its pipe, or deadlocked against us --
// see the workspace/configuration-during-initialize note in
// serverhandler.go) into an ordinary "this server failed to initialize"
// drop via the existing per-server error handling below, instead of an
// unbounded hang that takes the whole gateway down with it. No config
// knob for this: add one if and when someone actually hits the limit.
//
// A var, not a const, solely so tests can shrink it for the duration of
// a single test (see TestSession_InitializeTimeoutDropsHungServer)
// instead of a real test run waiting out the production default.
var downstreamInitializeTimeout = 45 * time.Second

// initializeParams is the subset of an "initialize" request's params
// this package needs to decode. Everything else is passed through
// verbatim via the raw json.RawMessage (see buildDownstreamInitParams).
type initializeParams struct {
	RootURI          *string `json:"rootUri"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
}

// handleInitialize implements the upstream "initialize" request. It is
// handled fully synchronously: nothing else may proceed until this
// completes, because it is what starts every downstream server.
func (s *Session) handleInitialize(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if m.ID == nil {
		return
	}

	var p initializeParams
	if len(m.Params) > 0 {
		if err := json.Unmarshal(m.Params, &p); err != nil {
			s.log.Warn("initialize: failed to decode known fields", "error", err)
		}
	}
	rootPath := deriveRootPath(p)

	rawParams := make(map[string]json.RawMessage)
	if len(m.Params) > 0 {
		if err := json.Unmarshal(m.Params, &rawParams); err != nil {
			s.log.Error("initialize: failed to decode params as object", "error", err)
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeInvalidParams,
				Message: fmt.Sprintf("initialize: invalid params: %v", err),
			})
			return
		}
	}

	// Establish downstream-facing state before any downstream can talk to
	// us: a server that issues a request during its own "initialize" (see
	// serverhandler.go) must see a Session whose root is already set, not
	// one still at its zero value.
	s.mu.Lock()
	s.rootPath = rootPath
	s.mu.Unlock()

	started, capsList := s.startServers(ctx, rawParams)

	if len(started) == 0 {
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: "rolling-star: no configured server could be started and initialized",
		})
		return
	}

	merged, err := mergeCapabilities(capsList)
	if err != nil {
		s.log.Error("initialize: failed to merge capabilities", "error", err)
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to merge capabilities: %v", err),
		})
		return
	}

	s.mu.Lock()
	s.servers = started
	s.mu.Unlock()

	result, err := json.Marshal(map[string]json.RawMessage{
		"capabilities": merged,
		"serverInfo":   json.RawMessage(`{"name":"rolling-star","version":"0.1.0"}`),
	})
	if err != nil {
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()})
		return
	}
	_ = c.Reply(*m.ID, result, nil)
}

// startServers launches and initializes every configured server
// concurrently -- each server's own startup (process launch, then its
// "initialize" round trip) is independent of every other server's, so
// running them serially would make a client's "initialize" latency the
// sum of every language server's cold-start time instead of the slowest
// one. Results are written into pre-sized slices indexed by cfg.Servers'
// position, then compacted: the index-preserving write is load-bearing,
// since mergeCapabilities' first-writer-wins tie-break depends on config
// order (see docs/v1-notes.md).
func (s *Session) startServers(ctx context.Context, rawParams map[string]json.RawMessage) (started []*Downstream, capsList []json.RawMessage) {
	servers := make([]*Downstream, len(s.cfg.Servers))
	caps := make([]json.RawMessage, len(s.cfg.Servers))

	var wg sync.WaitGroup
	for i, def := range s.cfg.Servers {
		wg.Go(func() {
			params, err := buildDownstreamInitParams(rawParams, def)
			if err != nil {
				s.log.Error("initialize: failed to build downstream params", "server", def.Name, "error", err)
				return
			}

			d, err := StartDownstream(ctx, def, s.launch)
			if err != nil {
				s.log.Error("initialize: failed to start server", "server", def.Name, "error", err)
				return
			}
			// Wire the server->client direction now that d exists. Same
			// package, so setting the unexported field directly is
			// simplest; see Downstream.Handle's doc comment for what it
			// does.
			d.sess = s

			initCtx, initCancel := context.WithTimeout(ctx, downstreamInitializeTimeout)
			serverCaps, err := d.Initialize(initCtx, params)
			initCancel()
			if err != nil {
				s.log.Error("initialize: server failed to initialize", "server", def.Name, "error", err)
				_ = d.Terminate()
				return
			}

			// Each goroutine owns a distinct index, so writing here needs
			// no lock: there is no concurrent access to the same slot.
			servers[i] = d
			caps[i] = serverCaps
		})
	}
	wg.Wait()

	for i, d := range servers {
		if d != nil {
			started = append(started, d)
			capsList = append(capsList, caps[i])
		}
	}
	return started, capsList
}

// deriveRootPath picks the session's root path from initialize params:
// the first workspace folder's URI, else rootUri, else the current
// working directory -- in that order, and only if non-empty.
func deriveRootPath(p initializeParams) string {
	if len(p.WorkspaceFolders) > 0 && p.WorkspaceFolders[0].URI != "" {
		return uriToPath(p.WorkspaceFolders[0].URI)
	}
	if p.RootURI != nil && *p.RootURI != "" {
		return uriToPath(*p.RootURI)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// uriToPath converts a file: URI to a filesystem path via
// router.FilePath, the same parse internal/router.PathForRouting uses
// internally. Non-file URIs and unparsable input are returned unchanged,
// since a root path value of "" would flow into a broken filepath.Rel
// call regardless.
func uriToPath(uri string) string {
	path, ok := router.FilePath(uri)
	if !ok {
		return uri
	}
	return path
}

// buildDownstreamInitParams derives the initialize params sent to one
// downstream server from the upstream's raw params: processId is
// overridden to our own pid, and initializationOptions is replaced with
// def's configured value (removed entirely if unset). Every other field
// -- rootUri, workspaceFolders, capabilities, clientInfo, ... -- passes
// through verbatim, since we forward everything the client can actually
// do.
func buildDownstreamInitParams(raw map[string]json.RawMessage, def config.ServerDef) (json.RawMessage, error) {
	out := maps.Clone(raw)

	pid, err := json.Marshal(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal processId: %w", err)
	}
	out["processId"] = pid

	if def.InitializationOptions != nil {
		opts, err := json.Marshal(def.InitializationOptions)
		if err != nil {
			return nil, fmt.Errorf("gateway: marshal initializationOptions for %s: %w", def.Name, err)
		}
		out["initializationOptions"] = opts
	} else {
		delete(out, "initializationOptions")
	}

	params, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal initialize params for %s: %w", def.Name, err)
	}
	return params, nil
}

// handleInitialized broadcasts "initialized" to every started server.
func (s *Session) handleInitialized() {
	for _, d := range s.snapshotServers() {
		if err := d.Conn().Notify("initialized", json.RawMessage(`{}`)); err != nil {
			s.log.Error("initialized: failed to notify server", "server", d.Def.Name, "error", err)
		}
	}
}

// handleShutdown fans "shutdown" out to every server, waits for all of
// them (bounded by shutdownTimeout), marks the session as shut down
// (this is the ONLY place that happens -- see ShutdownReceived's doc
// comment for why Close/shutdownAll must not also set it), and replies
// to the upstream request.
func (s *Session) handleShutdown(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	s.shutdownAll(ctx)

	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()

	if m.ID != nil {
		_ = c.Reply(*m.ID, nil, nil)
	}
}

// shutdownAll fans a "shutdown" request out to every currently-running
// server and waits for all of them (bounded by shutdownTimeout). It is
// the shared body behind both the protocol-driven handleShutdown and the
// out-of-band Close (e.g. on SIGINT/SIGTERM, where no upstream
// "shutdown" request was ever received to reply to).
//
// Deliberately does NOT set s.shutdown: that flag means "the upstream
// client sent us shutdown", which is what ShutdownReceived reports to
// choose an LSP-correct exit code. Close() also calls this (to shut
// downstreams down cleanly on a signal) without the client ever having
// sent "shutdown"; if this function set the flag, an interrupted process
// would misreport exit code 0. Only handleShutdown sets it.
func (s *Session) shutdownAll(ctx context.Context) {
	servers := s.snapshotServers()

	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, d := range servers {
		wg.Go(func() {
			if err := d.Shutdown(shutdownCtx); err != nil {
				s.log.Error("shutdown: server failed to shut down", "server", d.Def.Name, "error", err)
			}
		})
	}
	wg.Wait()
}

// exitAll sends "exit" to every currently-running server, then gives
// each a grace period to exit on its own before force-terminating it.
// Shared body behind handleExit (see Session.Handle) and Close.
func (s *Session) exitAll() {
	servers := s.snapshotServers()

	var wg sync.WaitGroup
	for _, d := range servers {
		wg.Go(func() {
			if err := d.Conn().Notify("exit", nil); err != nil {
				s.log.Warn("exit: failed to notify server", "server", d.Def.Name, "error", err)
			}

			waited := make(chan struct{})
			go func() {
				_ = d.Wait()
				close(waited)
			}()

			select {
			case <-waited:
				// The common, well-behaved case: the server saw "exit"
				// and quit on its own before the grace period elapsed.
				// Nothing left to do -- terminating an already-exited
				// process would just reconstruct, via the resulting
				// errno, the same fact this select arm already knows.
			case <-time.After(exitGracePeriod):
				if err := d.Terminate(); err != nil {
					s.log.Warn("exit: failed to terminate server", "server", d.Def.Name, "error", err)
				}
			}
		})
	}
	wg.Wait()
}

// Close tears every currently-running downstream server down: "shutdown"
// then "exit" then (after a grace period) a forced kill for any that
// haven't exited on their own, exactly like the protocol-driven path but
// without an upstream request/notification to answer. It is meant for
// out-of-band teardown -- main's wiring calls this on SIGINT/SIGTERM so
// an interrupted rolling-star doesn't leave orphaned child language
// server processes behind. Calling Close after the session has already
// shut down via the normal protocol path is safe: shutdownAll/exitAll
// simply iterate an empty server list at that point.
func (s *Session) Close(ctx context.Context) {
	s.shutdownAll(ctx)
	s.exitAll()
}

// snapshotServers returns a copy of the current server list, safe to
// range over without holding s.mu.
func (s *Session) snapshotServers() []*Downstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.servers)
}
