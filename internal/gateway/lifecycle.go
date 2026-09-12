package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// shutdownTimeout bounds how long handleShutdown waits for all
// downstream servers to answer "shutdown" before giving up on the slow
// ones and replying to the upstream client anyway.
const shutdownTimeout = 5 * time.Second

// exitGracePeriod is how long handleExit waits for a downstream process
// to exit on its own, after sending it "exit" and closing its stdin,
// before force-terminating it.
const exitGracePeriod = 2 * time.Second

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

	var started []*Downstream
	var capsList []json.RawMessage

	for _, def := range s.cfg.Servers {
		params, err := buildDownstreamInitParams(rawParams, def)
		if err != nil {
			s.log.Error("initialize: failed to build downstream params", "server", def.Name, "error", err)
			continue
		}

		d, err := StartDownstream(ctx, def, s.launch, nil)
		if err != nil {
			s.log.Error("initialize: failed to start server", "server", def.Name, "error", err)
			continue
		}

		if _, err := d.Initialize(ctx, params); err != nil {
			s.log.Error("initialize: server failed to initialize", "server", def.Name, "error", err)
			_ = d.Terminate()
			continue
		}

		started = append(started, d)
		capsList = append(capsList, d.Capabilities())
	}

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
	s.rootPath = rootPath
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

// uriToPath converts a file: URI to a filesystem path, decoding percent
// escapes via url.Parse (the same parse internal/router.PathForRouting
// uses internally) rather than reimplementing percent-decoding. Non-file
// URIs and unparsable input are returned unchanged, since a root path
// value of "" would flow into a broken filepath.Rel call regardless.
func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	return u.Path
}

// buildDownstreamInitParams derives the initialize params sent to one
// downstream server from the upstream's raw params: processId is
// overridden to our own pid, and initializationOptions is replaced with
// def's configured value (removed entirely if unset). Every other field
// -- rootUri, workspaceFolders, capabilities, clientInfo, ... -- passes
// through verbatim, since we forward everything the client can actually
// do.
func buildDownstreamInitParams(raw map[string]json.RawMessage, def config.ServerDef) (json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		out[k] = v
	}

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
		if err := d.Initialized(); err != nil {
			s.log.Error("initialized: failed to notify server", "server", d.Def.Name, "error", err)
		}
	}
}

// handleShutdown fans "shutdown" out to every server, waits for all of
// them (bounded by shutdownTimeout), marks the session as shut down, and
// replies to the upstream request.
func (s *Session) handleShutdown(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	servers := s.snapshotServers()

	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, d := range servers {
		wg.Add(1)
		go func(d *Downstream) {
			defer wg.Done()
			if err := d.Shutdown(shutdownCtx); err != nil {
				s.log.Error("shutdown: server failed to shut down", "server", d.Def.Name, "error", err)
			}
		}(d)
	}
	wg.Wait()

	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()

	if m.ID != nil {
		_ = c.Reply(*m.ID, json.RawMessage("null"), nil)
	}
}

// handleExit sends "exit" to every server, then gives each a grace
// period to exit on its own before force-terminating it.
func (s *Session) handleExit() {
	servers := s.snapshotServers()

	var wg sync.WaitGroup
	for _, d := range servers {
		wg.Add(1)
		go func(d *Downstream) {
			defer wg.Done()
			if err := d.Exit(); err != nil {
				s.log.Warn("exit: failed to notify server", "server", d.Def.Name, "error", err)
			}

			waited := make(chan struct{})
			go func() {
				_ = d.Wait()
				close(waited)
			}()

			select {
			case <-waited:
			case <-time.After(exitGracePeriod):
			}
			if err := d.Terminate(); err != nil {
				s.log.Warn("exit: failed to terminate server", "server", d.Def.Name, "error", err)
			}
		}(d)
	}
	wg.Wait()
}

// snapshotServers returns a copy of the current server list, safe to
// range over without holding s.mu.
func (s *Session) snapshotServers() []*Downstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Downstream, len(s.servers))
	copy(out, s.servers)
	return out
}
