package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/lsp"
	"github.com/cross-ts/rolling-star/internal/router"
)

type languageServerFactory func(context.Context, lsp.ServerDefinition) (*lsp.Server, error)

var errClientDisconnectedBeforeShutdown = errors.New("gateway: client disconnected before shutdown")

type Gateway struct {
	definitions         []config.LanguageServer
	router              *router.Router
	startLanguageServer languageServerFactory

	client *lsp.Client

	mu              sync.Mutex
	languageServers []*lsp.Server
	documentServers map[string]*lsp.Server
	rootPath        string
	shutdown        bool
}

func New(definitions []config.LanguageServer) (*Gateway, error) {
	var rules []router.Rule
	for _, def := range definitions {
		for _, sel := range def.Selectors {
			rules = append(rules, router.Rule{
				Server:   def.Name,
				Language: sel.Language,
				Pattern:  sel.Pattern,
			})
		}
	}

	r, err := router.New(rules)
	if err != nil {
		return nil, fmt.Errorf("gateway: create router: %w", err)
	}

	return &Gateway{
		definitions:         slices.Clone(definitions),
		router:              r,
		startLanguageServer: lsp.Start,
		documentServers:     make(map[string]*lsp.Server),
	}, nil
}

func (g *Gateway) Serve(ctx context.Context, client *lsp.Client) error {
	g.client = client
	if err := client.Run(ctx, func(m *jsonrpc.Message) {
		g.handleClientEvent(ctx, m)
	}); err != nil {
		return err
	}

	g.mu.Lock()
	shutdown := g.shutdown
	g.mu.Unlock()
	if !shutdown {
		return errClientDisconnectedBeforeShutdown
	}
	return nil
}

func (g *Gateway) handleClientEvent(ctx context.Context, m *jsonrpc.Message) {
	switch m.Method {
	case "initialize":
		g.handleInitialize(ctx, m)
	case "initialized":
		g.handleInitialized()
	case "shutdown":
		g.handleShutdown(ctx, m)
	case "exit":
		g.exitAll()
	default:
		g.route(m)
	}
}

func (g *Gateway) handleLanguageServerEvent(server *lsp.Server, m *jsonrpc.Message) {
	if !m.IsRequest() {
		if err := g.client.Notify(m.Method, m.Params); err != nil {
			slog.Error("language server notification failed", "server", server.Name(), "method", m.Method, "error", err)
		}
		return
	}

	ch, err := g.client.Call(m.Method, m.Params)
	if err != nil {
		_ = server.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to forward %s to client: %v", m.Method, err),
		})
		return
	}

	id := *m.ID
	go func() {
		msg := <-ch
		_ = server.Reply(id, msg.Result, msg.Error)
	}()
}
