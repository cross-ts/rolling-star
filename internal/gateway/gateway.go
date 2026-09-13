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

type Option func(*Gateway)

func WithLauncher(launcher Launcher) Option {
	return func(g *Gateway) {
		if launcher != nil {
			g.launch = launcher
		}
	}
}

func WithLogger(logger *slog.Logger) Option {
	return func(g *Gateway) {
		if logger != nil {
			g.log = logger
		}
	}
}

type languageServerMessage struct {
	conn    *jsonrpc.Conn
	message *jsonrpc.Message
}

type Gateway struct {
	definitions []config.LanguageServer
	router      *router.Router
	log         *slog.Logger
	launch      Launcher

	client *jsonrpc.Conn

	mu                     sync.Mutex
	languageServers        []*LanguageServer
	documentServers        map[string]*LanguageServer
	rootPath               string
	shutdown               bool
	languageServerMessages chan languageServerMessage
}

func New(definitions []config.LanguageServer, options ...Option) (*Gateway, error) {
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

	g := &Gateway{
		definitions:            slices.Clone(definitions),
		router:                 r,
		launch:                 ExecLauncher,
		documentServers:        make(map[string]*LanguageServer),
		languageServerMessages: make(chan languageServerMessage),
	}
	for _, option := range options {
		if option != nil {
			option(g)
		}
	}
	if g.log == nil {
		g.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return g, nil
}

func (g *Gateway) Serve(ctx context.Context, transport io.ReadWriteCloser) error {
	g.client = jsonrpc.NewConn(transport)
	clientDone := make(chan error, 1)
	go func() { clientDone <- g.client.Run(ctx) }()

	for {
		select {
		case m, ok := <-g.client.Messages():
			if !ok {
				return <-clientDone
			}
			g.handleClientMessage(ctx, g.client, m)
		case event := <-g.languageServerMessages:
			g.handleLanguageServerMessage(event)
		case err := <-clientDone:
			return err
		case <-ctx.Done():
			_ = g.client.Close()
			return ctx.Err()
		}
	}
}

func (g *Gateway) ShutdownReceived() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.shutdown
}

func (g *Gateway) handleClientMessage(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
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

func (g *Gateway) handleLanguageServerMessage(event languageServerMessage) {
	relay(g.log, event.conn, g.client, event.message, "client")
}
