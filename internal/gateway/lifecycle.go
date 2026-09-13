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
	"github.com/cross-ts/rolling-star/internal/languageserver"
	"github.com/cross-ts/rolling-star/internal/router"
)

const shutdownTimeout = 5 * time.Second

const exitGracePeriod = 2 * time.Second

var languageServerInitializeTimeout = 45 * time.Second

type initializeParams struct {
	RootURI          *string `json:"rootUri"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
}

func (g *Gateway) handleInitialize(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if m.ID == nil {
		return
	}

	var p initializeParams
	if len(m.Params) > 0 {
		if err := json.Unmarshal(m.Params, &p); err != nil {
			g.log.Warn("initialize: failed to decode known fields", "error", err)
		}
	}
	rootPath := deriveRootPath(p)

	rawParams := make(map[string]json.RawMessage)
	if len(m.Params) > 0 {
		if err := json.Unmarshal(m.Params, &rawParams); err != nil {
			g.log.Error("initialize: failed to decode params as object", "error", err)
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeInvalidParams,
				Message: fmt.Sprintf("initialize: invalid params: %v", err),
			})
			return
		}
	}

	g.mu.Lock()
	g.rootPath = rootPath
	g.mu.Unlock()

	started, capsList := g.startLanguageServers(ctx, rawParams)

	if len(started) == 0 {
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: "rolling-star: no configured server could be started and initialized",
		})
		return
	}

	merged, err := mergeCapabilities(capsList)
	if err != nil {
		g.log.Error("initialize: failed to merge capabilities", "error", err)
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to merge capabilities: %v", err),
		})
		return
	}

	g.mu.Lock()
	g.languageServers = started
	g.mu.Unlock()

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

func (g *Gateway) startLanguageServers(ctx context.Context, rawParams map[string]json.RawMessage) (started []*languageserver.Server, capsList []json.RawMessage) {
	servers := make([]*languageserver.Server, len(g.definitions))
	caps := make([]json.RawMessage, len(g.definitions))

	var wg sync.WaitGroup
	for i, def := range g.definitions {
		wg.Go(func() {
			params, err := buildLanguageServerInitParams(rawParams)
			if err != nil {
				g.log.Error("initialize: failed to build language server params", "server", def.Name, "error", err)
				return
			}

			server, err := g.startLanguageServer(ctx, def)
			if err != nil {
				g.log.Error("initialize: failed to start server", "server", def.Name, "error", err)
				return
			}
			go func() {
				if err := server.Run(ctx, func(m *jsonrpc.Message) {
					g.handleLanguageServerEvent(server, m)
				}); err != nil {
					g.log.Error("language server connection ended with an error", "server", server.Name(), "error", err)
				}
			}()

			initCtx, initCancel := context.WithTimeout(ctx, languageServerInitializeTimeout)
			serverCaps, err := server.Initialize(initCtx, params)
			initCancel()
			if err != nil {
				g.log.Error("initialize: server failed to initialize", "server", def.Name, "error", err)
				_ = server.Terminate()
				return
			}

			servers[i] = server
			caps[i] = serverCaps
		})
	}
	wg.Wait()

	for i, server := range servers {
		if server != nil {
			started = append(started, server)
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

func buildLanguageServerInitParams(raw map[string]json.RawMessage) (json.RawMessage, error) {
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

func (g *Gateway) handleInitialized() {
	for _, server := range g.snapshotLanguageServers() {
		if err := server.Conn().Notify("initialized", json.RawMessage(`{}`)); err != nil {
			g.log.Error("initialized: failed to notify server", "server", server.Name(), "error", err)
		}
	}
}

func (g *Gateway) handleShutdown(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	g.shutdownAll(ctx)

	g.mu.Lock()
	g.shutdown = true
	g.mu.Unlock()

	if m.ID != nil {
		_ = c.Reply(*m.ID, nil, nil)
	}
}

func (g *Gateway) shutdownAll(ctx context.Context) {
	servers := g.snapshotLanguageServers()

	shutdownCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Go(func() {
			if err := server.Shutdown(shutdownCtx); err != nil {
				g.log.Error("shutdown: server failed to shut down", "server", server.Name(), "error", err)
			}
		})
	}
	wg.Wait()
}

func (g *Gateway) exitAll() {
	servers := g.snapshotLanguageServers()

	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Go(func() {
			if err := server.Conn().Notify("exit", nil); err != nil {
				g.log.Warn("exit: failed to notify server", "server", server.Name(), "error", err)
			}

			waited := make(chan struct{})
			go func() {
				_ = server.Wait()
				close(waited)
			}()

			select {
			case <-waited:

			case <-time.After(exitGracePeriod):
				if err := server.Terminate(); err != nil {
					g.log.Warn("exit: failed to terminate server", "server", server.Name(), "error", err)
				}
			}
		})
	}
	wg.Wait()
}

func (g *Gateway) snapshotLanguageServers() []*languageserver.Server {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.languageServers)
}
