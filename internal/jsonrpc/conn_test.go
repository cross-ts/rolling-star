package jsonrpc

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// recordingHandler records every notification/request it receives, in
// order, and can be configured to auto-reply to requests.
type recordingHandler struct {
	mu       sync.Mutex
	received []*Message

	// reply, if non-nil, is invoked for every request to compute a
	// result to send back via c.Reply.
	reply func(m *Message) (json.RawMessage, *Error)
}

func (h *recordingHandler) Handle(ctx context.Context, c *Conn, m *Message) {
	h.mu.Lock()
	h.received = append(h.received, m)
	h.mu.Unlock()

	if m.IsRequest() && h.reply != nil {
		result, errObj := h.reply(m)
		_ = c.Reply(*m.ID, result, errObj)
	}
}

func (h *recordingHandler) snapshot() []*Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Message, len(h.received))
	copy(out, h.received)
	return out
}

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newPipeConns(t *testing.T, serverHandler, clientHandler Handler) (server, client *Conn) {
	t.Helper()
	a, b := net.Pipe()
	server = NewConn(a, serverHandler)
	client = NewConn(b, clientHandler)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return server, client
}

func TestConnCallAndReply(t *testing.T) {
	serverH := &recordingHandler{
		reply: func(m *Message) (json.RawMessage, *Error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	}
	server, client := newPipeConns(t, serverH, nil)

	ctx := withTimeout(t)
	go func() { _ = server.Run(ctx) }()
	go func() { _ = client.Run(ctx) }()

	ch, err := client.Call("ping", json.RawMessage(`{"n":1}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			t.Fatalf("unexpected error response: %+v", resp.Error)
		}
		if string(resp.Result) != `{"ok":true}` {
			t.Fatalf("unexpected result: %s", resp.Result)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for response")
	}
}

func TestConnConcurrentCallsGetDistinctIDsAndCorrectResponses(t *testing.T) {
	serverH := &recordingHandler{
		reply: func(m *Message) (json.RawMessage, *Error) {
			// Echo the params back as the result so we can verify
			// correlation, not just "a" response arrived.
			return m.Params, nil
		},
	}
	server, client := newPipeConns(t, serverH, nil)

	ctx := withTimeout(t)
	go func() { _ = server.Run(ctx) }()
	go func() { _ = client.Run(ctx) }()

	const n = 10
	chans := make([]<-chan *Message, n)
	for i := 0; i < n; i++ {
		params := json.RawMessage(`{"i":` + strconv.Itoa(i) + `}`)
		ch, err := client.Call("echo", params)
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		chans[i] = ch
	}

	for i := 0; i < n; i++ {
		select {
		case resp := <-chans[i]:
			want := `{"i":` + strconv.Itoa(i) + `}`
			if string(resp.Result) != want {
				t.Fatalf("call %d: got result %s want %s", i, resp.Result, want)
			}
		case <-ctx.Done():
			t.Fatalf("call %d: timed out", i)
		}
	}
}

func TestConnNotificationOrderingPreservedRelativeToCalls(t *testing.T) {
	serverH := &recordingHandler{
		reply: func(m *Message) (json.RawMessage, *Error) {
			return json.RawMessage(`{}`), nil
		},
	}
	server, client := newPipeConns(t, serverH, nil)

	ctx := withTimeout(t)
	go func() { _ = server.Run(ctx) }()
	go func() { _ = client.Run(ctx) }()

	// Send didOpen (notification), then hover (call), from the same
	// goroutine: order on the wire must be preserved, and the handler
	// (a stand-in for Session.Handle) must see them in that order since
	// Run dispatches synchronously.
	if err := client.Notify("textDocument/didOpen", json.RawMessage(`{"doc":"a"}`)); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	ch, err := client.Call("textDocument/hover", json.RawMessage(`{"doc":"a"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal("timed out waiting for hover response")
	}

	received := serverH.snapshot()
	if len(received) != 2 {
		t.Fatalf("expected 2 messages received, got %d", len(received))
	}
	if received[0].Method != "textDocument/didOpen" {
		t.Fatalf("expected didOpen first, got %s", received[0].Method)
	}
	if received[1].Method != "textDocument/hover" {
		t.Fatalf("expected hover second, got %s", received[1].Method)
	}
}

func TestConnErrorResponseRoundTrip(t *testing.T) {
	serverH := &recordingHandler{
		reply: func(m *Message) (json.RawMessage, *Error) {
			return nil, &Error{
				Code:    CodeMethodNotFound,
				Message: "no such method",
				Data:    json.RawMessage(`{"method":"nope"}`),
			}
		},
	}
	server, client := newPipeConns(t, serverH, nil)

	ctx := withTimeout(t)
	go func() { _ = server.Run(ctx) }()
	go func() { _ = client.Run(ctx) }()

	ch, err := client.Call("nope", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	select {
	case resp := <-ch:
		if resp.Error == nil {
			t.Fatal("expected error response")
		}
		if resp.Error.Code != CodeMethodNotFound {
			t.Fatalf("got code %d want %d", resp.Error.Code, CodeMethodNotFound)
		}
		if resp.Error.Message != "no such method" {
			t.Fatalf("got message %q", resp.Error.Message)
		}
		if string(resp.Error.Data) != `{"method":"nope"}` {
			t.Fatalf("got data %s", resp.Error.Data)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for error response")
	}
}

func TestConnReplyNullResultOverWire(t *testing.T) {
	serverH := &recordingHandler{
		reply: func(m *Message) (json.RawMessage, *Error) {
			return nil, nil // nil result, no error: should become "result": null
		},
	}
	server, client := newPipeConns(t, serverH, nil)

	ctx := withTimeout(t)
	go func() { _ = server.Run(ctx) }()
	go func() { _ = client.Run(ctx) }()

	ch, err := client.Call("shutdown", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			t.Fatalf("unexpected error: %+v", resp.Error)
		}
		if resp.Result == nil {
			t.Fatal("expected non-nil (null) result")
		}
		if string(resp.Result) != "null" {
			t.Fatalf("expected result null, got %s", resp.Result)
		}
	case <-ctx.Done():
		t.Fatal("timed out")
	}
}

func TestConnPendingCallsReleasedWhenPeerCloses(t *testing.T) {
	server, client := newPipeConns(t, nil, nil)

	ctx := withTimeout(t)
	go func() { _ = server.Run(ctx) }()
	// client.Run must be running so that server's Call (a blocking
	// net.Pipe write) actually completes; client's read loop just
	// discards the request since it has no handler configured. We then
	// close the client to simulate the peer going away and verify the
	// server's still-pending call gets released rather than hanging.
	go func() { _ = client.Run(ctx) }()

	ch, err := server.Call("willNeverReply", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	// Closing the client's end of the pipe causes the server's read loop
	// to see an error/EOF, which must release pending calls.
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case resp := <-ch:
		if resp == nil {
			t.Fatal("expected a synthetic error message, got nil (closed channel with no value)")
		}
		if resp.Error == nil {
			t.Fatal("expected synthetic error response after peer close")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for pending call release")
	}

	select {
	case <-server.Done():
	case <-ctx.Done():
		t.Fatal("timed out waiting for Done() to close")
	}
}
