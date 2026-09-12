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

// ServerRequestHandler answers a custom request a downstream server sent
// us, without forwarding it to the upstream client. This is the deferred
// "handler table" hook point described in the design: in v1
// Session.serverHandlers is always empty (no entries are ever
// registered), but the dispatch shape exists so a later change can add
// entries (e.g. for actions/readFile, or answering workspace/configuration
// locally) without touching the core forwarding loop.
type ServerRequestHandler func(ctx context.Context, d *Downstream, params json.RawMessage) (json.RawMessage, *jsonrpc.Error)

// Options configures a Session.
type Options struct {
	// Launcher starts downstream servers. If nil, ExecLauncher is used.
	Launcher Launcher

	// Logger receives all Session diagnostics. All logging MUST go
	// through this logger (to stderr in production) -- nothing but the
	// upstream connection's framing writer may write to stdout. If nil,
	// logs are discarded.
	Logger *slog.Logger
}

// Session is a single upstream connection's worth of gateway state: it
// behaves as one Language Server to the upstream client, fans out to the
// configured downstream servers, and (once routing is implemented)
// forwards document traffic between them based on internal/router's
// selector matching.
type Session struct {
	cfg    *config.Config
	router *router.Router
	log    *slog.Logger
	launch Launcher

	upstream *jsonrpc.Conn

	mu       sync.Mutex
	servers  []*Downstream          // config order; populated by initialize (§4.2)
	docs     map[string]*Downstream // documentURI -> bound server; populated in T6
	rootPath string
	shutdown bool

	// serverHandlers is always empty in v1; see ServerRequestHandler.
	serverHandlers map[string]ServerRequestHandler
}

// New builds a Session from cfg. It flattens cfg.Servers[].Selectors into
// router.Rule entries in config order -- this is where the config's
// first-match-wins ordering becomes the router's ordering -- and
// compiles them into a router.Router.
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

// Serve runs the upstream connection over transport until the connection
// ends (the peer disconnects, or the underlying transport errors). It
// blocks for the lifetime of the session.
func (s *Session) Serve(ctx context.Context, transport io.ReadWriteCloser) error {
	s.upstream = jsonrpc.NewConn(transport, s)
	return s.upstream.Run(ctx)
}

// ShutdownReceived reports whether the upstream "shutdown" request has
// been handled, which callers (T7's main) use to choose an exit code:
// 0 if shutdown was received before exit, 1 otherwise (per the LSP
// spec's exit-without-shutdown case).
func (s *Session) ShutdownReceived() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shutdown
}

// Handle implements jsonrpc.Handler for the upstream connection. It is
// invoked synchronously from the upstream Conn's single read loop, so
// message ordering as sent by the client is preserved end to end.
func (s *Session) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "initialize":
		s.handleInitialize(ctx, c, m)
	case "initialized":
		s.handleInitialized()
	case "shutdown":
		s.handleShutdown(ctx, c, m)
	case "exit":
		s.handleExit()
	default:
		// T6: document routing and forwarding (client->server) and the
		// §4.4 policy table for document-context-less messages. Until
		// then, requests get MethodNotFound and notifications are
		// dropped, which is a safe (if unhelpful) default.
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: fmt.Sprintf("rolling-star: method not implemented: %s", m.Method),
			})
		}
	}
}
