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

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/router"
)

const shutdownTimeout = 5 * time.Second

const exitGracePeriod = 2 * time.Second

var downstreamInitializeTimeout = 45 * time.Second

type initializeParams struct {
	RootURI          *string `json:"rootUri"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
}

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

func (s *Session) startServers(ctx context.Context, rawParams map[string]json.RawMessage) (started []*Downstream, capsList []json.RawMessage) {
	servers := make([]*Downstream, len(s.cfg.Servers))
	caps := make([]json.RawMessage, len(s.cfg.Servers))

	var wg sync.WaitGroup
	for i, def := range s.cfg.Servers {
		wg.Go(func() {
			params, err := buildDownstreamInitParams(rawParams)
			if err != nil {
				s.log.Error("initialize: failed to build downstream params", "server", def.Name, "error", err)
				return
			}

			d, err := StartDownstream(ctx, def, s.launch)
			if err != nil {
				s.log.Error("initialize: failed to start server", "server", def.Name, "error", err)
				return
			}

			d.sess = s

			initCtx, initCancel := context.WithTimeout(ctx, downstreamInitializeTimeout)
			serverCaps, err := d.Initialize(initCtx, params)
			initCancel()
			if err != nil {
				s.log.Error("initialize: server failed to initialize", "server", def.Name, "error", err)
				_ = d.Terminate()
				return
			}

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

func uriToPath(uri string) string {
	path, ok := router.FilePath(uri)
	if !ok {
		return uri
	}
	return path
}

func buildDownstreamInitParams(raw map[string]json.RawMessage) (json.RawMessage, error) {
	out := maps.Clone(raw)

	pid, err := json.Marshal(os.Getpid())
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal processId: %w", err)
	}
	out["processId"] = pid

	params, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("gateway: marshal initialize params: %w", err)
	}
	return params, nil
}

func (s *Session) handleInitialized() {
	for _, d := range s.snapshotServers() {
		if err := d.Conn().Notify("initialized", json.RawMessage(`{}`)); err != nil {
			s.log.Error("initialized: failed to notify server", "server", d.Def.Name, "error", err)
		}
	}
}

func (s *Session) handleShutdown(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	s.shutdownAll(ctx)

	s.mu.Lock()
	s.shutdown = true
	s.mu.Unlock()

	if m.ID != nil {
		_ = c.Reply(*m.ID, nil, nil)
	}
}

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

			case <-time.After(exitGracePeriod):
				if err := d.Terminate(); err != nil {
					s.log.Warn("exit: failed to terminate server", "server", d.Def.Name, "error", err)
				}
			}
		})
	}
	wg.Wait()
}

func (s *Session) Close(ctx context.Context) {
	s.shutdownAll(ctx)
	s.exitAll()
}

func (s *Session) snapshotServers() []*Downstream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.servers)
}
