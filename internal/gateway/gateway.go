package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/cross-ts/rolling-star/internal/client"
	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/languageserver"
	"github.com/cross-ts/rolling-star/internal/router"
)

type languageServerFactory func(context.Context, config.LanguageServer) (*languageserver.Server, error)

type Gateway struct {
	definitions         []config.LanguageServer
	router              *router.Router
	log                 *slog.Logger
	startLanguageServer languageServerFactory

	client *client.Client

	mu              sync.Mutex
	languageServers []*languageserver.Server
	documentServers map[string]*languageserver.Server
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
		log:                 slog.Default(),
		startLanguageServer: languageserver.Start,
		documentServers:     make(map[string]*languageserver.Server),
	}, nil
}

func (g *Gateway) Serve(ctx context.Context, client *client.Client) error {
	g.client = client
	return client.Run(ctx, func(m *jsonrpc.Message) {
		g.handleClientEvent(ctx, m)
	})
}

func (g *Gateway) ShutdownReceived() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shutdown
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

func (g *Gateway) handleLanguageServerEvent(server *languageserver.Server, m *jsonrpc.Message) {
	if !m.IsRequest() {
		if err := g.client.Notify(m.Method, m.Params); err != nil {
			g.log.Error("language server notification failed", "server", server.Name(), "method", m.Method, "error", err)
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
