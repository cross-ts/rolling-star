package languageserver

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type messageHandler interface {
	Handle(context.Context, *jsonrpc.Conn, *jsonrpc.Message)
}

func runMessages(ctx context.Context, conn *jsonrpc.Conn, handler messageHandler) error {
	done := make(chan error, 1)
	go func() { done <- conn.Run(ctx) }()

	for message := range conn.Messages() {
		if handler != nil {
			handler.Handle(ctx, conn, message)
		}
	}
	return <-done
}

type received struct {
	Method string
}

type fakeServer struct {
	caps    json.RawMessage
	initErr *jsonrpc.Error

	mu       sync.Mutex
	receipts []received
	conn     *jsonrpc.Conn
}

func newFakeServer(caps json.RawMessage) *fakeServer {
	return &fakeServer{caps: caps}
}

func (f *fakeServer) Handle(_ context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	f.mu.Lock()
	f.receipts = append(f.receipts, received{Method: m.Method})
	f.mu.Unlock()

	switch m.Method {
	case "initialize":
		if f.initErr != nil {
			_ = c.Reply(*m.ID, nil, f.initErr)
			return
		}
		result, _ := json.Marshal(map[string]json.RawMessage{"capabilities": f.caps})
		_ = c.Reply(*m.ID, result, nil)
	case "shutdown":
		_ = c.Reply(*m.ID, nil, nil)
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

func waitForReceipt(t *testing.T, fake *fakeServer, method string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if slices.ContainsFunc(fake.Received(), func(r received) bool { return r.Method == method }) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", method)
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

func newFakeLauncher(fs *fakeServer) launcher {
	return func(ctx context.Context, _ config.LanguageServer) (Process, error) {
		clientSide, serverSide := net.Pipe()
		fs.conn = jsonrpc.NewConn(serverSide)
		go func() { _ = runMessages(ctx, fs.conn, fs) }()
		return newPipeProcess(clientSide), nil
	}
}
