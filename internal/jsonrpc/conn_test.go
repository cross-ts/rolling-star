package jsonrpc

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

type recordingHandler struct {
	mu       sync.Mutex
	received []*Message

	reply func(m *Message) (json.RawMessage, *Error)
}

type messageHandler interface {
	Handle(context.Context, *Conn, *Message)
}

func runMessageHandler(ctx context.Context, conn *Conn, handler messageHandler) error {
	done := make(chan error, 1)
	go func() { done <- conn.Run(ctx) }()

	for message := range conn.Messages() {
		if handler != nil {
			handler.Handle(ctx, conn, message)
		}
	}
	return <-done
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
	return slices.Clone(h.received)
}

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newPipeConns(t *testing.T, serverHandler, clientHandler messageHandler) (server, client *Conn, ctx context.Context) {
	t.Helper()
	a, b := net.Pipe()
	server = NewConn(a)
	client = NewConn(b)
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})

	ctx = withTimeout(t)
	go func() { _ = runMessageHandler(ctx, server, serverHandler) }()
	go func() { _ = runMessageHandler(ctx, client, clientHandler) }()

	return server, client, ctx
}

func TestConnCallAndReply(t *testing.T) {
	serverH := &recordingHandler{
		reply: func(m *Message) (json.RawMessage, *Error) {
			return json.RawMessage(`{"ok":true}`), nil
		},
	}
	_, client, ctx := newPipeConns(t, serverH, nil)

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

			return m.Params, nil
		},
	}
	_, client, ctx := newPipeConns(t, serverH, nil)

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
	_, client, ctx := newPipeConns(t, serverH, nil)

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
	_, client, ctx := newPipeConns(t, serverH, nil)

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
			return nil, nil
		},
	}
	_, client, ctx := newPipeConns(t, serverH, nil)

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

	server, client, ctx := newPipeConns(t, nil, nil)

	ch, err := server.Call("willNeverReply", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

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
