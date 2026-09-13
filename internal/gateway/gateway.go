package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/router"
)

type Options struct {
	Launcher Launcher

	Logger *slog.Logger
}

type Gateway struct {
	definitions []config.LanguageServer
	router      *router.Router
	log         *slog.Logger
	launch      Launcher

	upstream *jsonrpc.Conn

	mu              sync.Mutex
	languageServers []*LanguageServer
	documentServers map[string]*LanguageServer
	rootPath        string
	shutdown        bool
}

func New(definitions []config.LanguageServer, opts Options) (*Gateway, error) {
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

	launch := opts.Launcher
	if launch == nil {
		launch = ExecLauncher
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Gateway{
		definitions:     slices.Clone(definitions),
		router:          r,
		log:             log,
		launch:          launch,
		documentServers: make(map[string]*LanguageServer),
	}, nil
}

func (g *Gateway) Serve(ctx context.Context, transport io.ReadWriteCloser) error {
	g.upstream = jsonrpc.NewConn(transport, g)
	return g.upstream.Run(ctx)
}

func (g *Gateway) ShutdownReceived() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shutdown
}

func (g *Gateway) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "initialize":
		g.handleInitialize(ctx, c, m)
	case "initialized":
		g.handleInitialized()
	case "shutdown":
		g.handleShutdown(ctx, c, m)
	case "exit":
		g.exitAll()
	default:
		g.route(c, m)
	}
}
