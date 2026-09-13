package gateway

import (
	"context"
	"encoding/json"
	"net"
	"sync"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// received is one (method, params) pair captured by a fakeServer.
type received struct {
	Method string
	Params json.RawMessage
}

// fakeServer is a jsonrpc.Handler standing in for a downstream language
// server in tests, so gateway logic can be exercised over a real
// net.Pipe connection without spawning any process. It records every
// message it receives, answers "initialize" with configurable
// capabilities, answers "textDocument/hover" with a canned response, and
// lets tests drive the server->client direction via PushDiagnostics and
// AskConfiguration.
type fakeServer struct {
	caps        json.RawMessage
	hoverResult json.RawMessage

	// initErr, if non-nil, makes this fake reply to "initialize" with an
	// error instead of caps, simulating a server that fails to initialize.
	initErr *jsonrpc.Error

	// hangOnInitialize, if true, makes this fake record "initialize" but
	// never reply to it at all, simulating a downstream that deadlocks or
	// never responds (see TestSession_InitializeTimeoutDropsHungServer).
	hangOnInitialize bool

	mu       sync.Mutex
	receipts []received
	conn     *jsonrpc.Conn // set once the pipe is established; see newFakeLauncher
}

func newFakeServer(caps json.RawMessage) *fakeServer {
	return &fakeServer{
		caps:        caps,
		hoverResult: json.RawMessage(`{"contents":"fake hover"}`),
	}
}

// Handle implements jsonrpc.Handler.
func (f *fakeServer) Handle(_ context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	f.mu.Lock()
	f.receipts = append(f.receipts, received{Method: m.Method, Params: m.Params})
	f.mu.Unlock()

	switch m.Method {
	case "initialize":
		if f.hangOnInitialize {
			return
		}
		if f.initErr != nil {
			_ = c.Reply(*m.ID, nil, f.initErr)
			return
		}
		result, _ := json.Marshal(map[string]json.RawMessage{"capabilities": f.caps})
		_ = c.Reply(*m.ID, result, nil)
	case "shutdown":
		_ = c.Reply(*m.ID, json.RawMessage("null"), nil)
	case "textDocument/hover":
		_ = c.Reply(*m.ID, f.hoverResult, nil)
	case "initialized", "exit":
		// Notifications; nothing to reply.
	default:
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: "fakeServer: method not implemented: " + m.Method,
			})
		}
	}
}

// Received returns a snapshot of every (method, params) this fake has
// seen so far.
func (f *fakeServer) Received() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]received, len(f.receipts))
	copy(out, f.receipts)
	return out
}

// PushDiagnostics sends a textDocument/publishDiagnostics notification
// from this fake to whatever is on the other end of its connection.
func (f *fakeServer) PushDiagnostics(uri string, diagnostics json.RawMessage) error {
	params, _ := json.Marshal(map[string]json.RawMessage{
		"uri":         mustMarshal(uri),
		"diagnostics": diagnostics,
	})
	return f.conn.Notify("textDocument/publishDiagnostics", params)
}

// AskConfiguration sends a workspace/configuration request from this
// fake, returning the channel the caller's response will arrive on.
func (f *fakeServer) AskConfiguration(items json.RawMessage) (<-chan *jsonrpc.Message, error) {
	return f.conn.Call("workspace/configuration", items)
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// pipeProcess adapts one end of a net.Pipe to Process, for use by fake
// Launchers in tests. net.Pipe's Write blocks until the peer Read's it,
// so callers must ensure the fakeServer's Conn.Run loop on the other end
// is already scheduled (a `go` statement is enough: Run does not need to
// have started, just to be guaranteed to start eventually) before
// writing to a pipeProcess.
type pipeProcess struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func newPipeProcess(c net.Conn) *pipeProcess {
	return &pipeProcess{Conn: c, done: make(chan struct{})}
}

func (p *pipeProcess) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.Conn.Close()
}

// Wait blocks until Close has been called. net.Pipe has no real process
// to wait on, so this is the closest fake equivalent.
func (p *pipeProcess) Wait() error {
	<-p.done
	return nil
}

// newFakeLauncher returns a Launcher that, instead of spawning a real
// process, wires a net.Pipe between the returned Process and a fresh
// fakeServer running its own jsonrpc.Conn on the other end. The
// fakeServer is returned so the test can inspect what it received and
// drive server->client traffic.
func newFakeLauncher(fs *fakeServer) Launcher {
	return func(ctx context.Context, def config.ServerDef) (Process, error) {
		clientSide, serverSide := net.Pipe()
		fs.conn = jsonrpc.NewConn(serverSide, fs)
		go fs.conn.Run(ctx) //nolint:errcheck // test harness; errors surface as test failures via missing responses
		return newPipeProcess(clientSide), nil
	}
}
