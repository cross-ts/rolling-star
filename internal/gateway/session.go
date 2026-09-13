package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/router"
)

type ServerRequestHandler func(ctx context.Context, d *Downstream, params json.RawMessage) (json.RawMessage, *jsonrpc.Error)

type Options struct {
	Launcher Launcher

	Logger *slog.Logger
}

type Session struct {
	cfg    *config.Config
	router *router.Router
	log    *slog.Logger
	launch Launcher

	upstream *jsonrpc.Conn

	mu       sync.Mutex
	servers  []*Downstream
	docs     map[string]*Downstream
	rootPath string
	shutdown bool

	serverHandlers map[string]ServerRequestHandler
}

func New(cfg *config.Config, opts Options) (*Session, error) {
	var rules []router.Rule
	for _, def := range cfg.Servers {
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
		return nil, fmt.Errorf("gateway: new session: %w", err)
	}

	launch := opts.Launcher
	if launch == nil {
		launch = ExecLauncher
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Session{
		cfg:            cfg,
		router:         r,
		log:            log,
		launch:         launch,
		docs:           make(map[string]*Downstream),
		serverHandlers: make(map[string]ServerRequestHandler),
	}, nil
}

func (s *Session) Serve(ctx context.Context, transport io.ReadWriteCloser) error {
	s.upstream = jsonrpc.NewConn(transport, s)
	return s.upstream.Run(ctx)
}

func (s *Session) ShutdownReceived() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown
}

func (s *Session) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "initialize":
		s.handleInitialize(ctx, c, m)
	case "initialized":
		s.handleInitialized()
	case "shutdown":
		s.handleShutdown(ctx, c, m)
	case "exit":
		s.exitAll()
	default:

		s.route(c, m)
	}
}
