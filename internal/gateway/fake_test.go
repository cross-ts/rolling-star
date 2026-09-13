package gateway

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"sync"
	"testing"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type received struct {
	Method string
	Params json.RawMessage
}

type fakeServer struct {
	caps        json.RawMessage
	hoverResult json.RawMessage

	initErr *jsonrpc.Error

	hangOnInitialize bool

	mu       sync.Mutex
	receipts []received
	conn     *jsonrpc.Conn
}

func newFakeServer(caps json.RawMessage) *fakeServer {
	return &fakeServer{
		caps:        caps,
		hoverResult: json.RawMessage(`{"contents":"fake hover"}`),
	}
}

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
		_ = c.Reply(*m.ID, nil, nil)
	case "textDocument/hover":
		_ = c.Reply(*m.ID, f.hoverResult, nil)
	case "initialized", "exit":

	default:
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: "fakeServer: method not implemented: " + m.Method,
			})
		}
	}
}

func (f *fakeServer) Received() []received {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.receipts)
}

func (f *fakeServer) methods() []string {
	recv := f.Received()
	out := make([]string, len(recv))
	for i, r := range recv {
		out[i] = r.Method
	}
	return out
}

func assertMethods(t *testing.T, fake *fakeServer, want ...string) {
	t.Helper()
	got := fake.methods()
	if !slices.Equal(got, want) {
		t.Fatalf("received %v, want %v", got, want)
	}
}

func (f *fakeServer) PushDiagnostics(uri string, diagnostics json.RawMessage) error {
	params, _ := json.Marshal(map[string]json.RawMessage{
		"uri":         mustMarshal(uri),
		"diagnostics": diagnostics,
	})
	return f.conn.Notify("textDocument/publishDiagnostics", params)
}

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

func (p *pipeProcess) Wait() error {
	<-p.done
	return nil
}

func newFakeLauncher(fs *fakeServer) Launcher {
	return func(ctx context.Context, def config.LanguageServer) (Process, error) {
		clientSide, serverSide := net.Pipe()
		fs.conn = jsonrpc.NewConn(serverSide, fs)
		go fs.conn.Run(ctx)
		return newPipeProcess(clientSide), nil
	}
}
