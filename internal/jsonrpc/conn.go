package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Handler processes an incoming notification or request. Handle is always
// invoked synchronously from Conn's single read loop (see Run), so
// ordering between messages is preserved: a handler MUST NOT be made
// asynchronous internally in a way that lets a later message's effects
// (e.g. a hover) overtake an earlier one (e.g. the didOpen that binds the
// document the hover targets).
type Handler interface {
	Handle(ctx context.Context, c *Conn, m *Message)
}

// Conn is a bidirectional JSON-RPC connection over an io.ReadWriteCloser.
// It multiplexes three roles on one wire: outgoing calls awaiting a
// response, incoming responses that resolve those calls, and incoming
// requests/notifications dispatched to a Handler.
type Conn struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	h   Handler

	readBuf []byte // scratch buffer reused across readFrame calls, see Run

	wmu      sync.Mutex // serializes writes to rwc
	writeBuf []byte     // scratch buffer reused across writeFrame calls, guarded by wmu

	seq atomic.Int64 // source of outgoing request ids

	mu      sync.Mutex
	pending map[ID]chan *Message // outgoing call id -> response channel
	closed  bool

	done chan struct{}
}

// NewConn creates a Conn over rwc. h may be nil if this side never
// expects to receive requests or notifications (in which case any that
// arrive are silently dropped).
func NewConn(rwc io.ReadWriteCloser, h Handler) *Conn {
	return &Conn{
		rwc:     rwc,
		r:       bufio.NewReader(rwc),
		h:       h,
		pending: make(map[ID]chan *Message),
		done:    make(chan struct{}),
	}
}

// Run executes the connection's single read loop until the peer closes
// the connection, the underlying reader errors, or ctx handling inside a
// Handler decides to stop calling back into Run (Run itself does not
// watch ctx directly; it is passed through to Handler.Handle so handlers
// can observe cancellation).
//
// Each frame read is dispatched in order:
//   - a response is resolved against the matching pending Call inline;
//   - a request or notification is passed to Handler.Handle synchronously,
//     before the next frame is read.
//
// Run returns nil on a clean EOF from the peer, and a wrapped error
// otherwise. In both cases, on exit, Done() is closed and every call
// still awaiting a response is released (see releasePending).
func (c *Conn) Run(ctx context.Context) error {
	defer c.releasePending()
	defer close(c.done)

	for {
		body, err := readFrame(c.r, c.readBuf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("jsonrpc: run: %w", err)
		}
		c.readBuf = body

		var m Message
		if err := json.Unmarshal(body, &m); err != nil {
			// Malformed frame: nothing sane to correlate it to, so drop
			// it rather than aborting the whole connection.
			continue
		}

		if m.IsResponse() {
			c.resolvePending(&m)
			continue
		}

		if c.h != nil {
			c.h.Handle(ctx, c, &m)
		}
	}
}

// Notify sends a notification (no id, no response expected). The send is
// synchronous with respect to other Notify/Call/Reply calls, so ordering
// relative to them is preserved on the wire.
func (c *Conn) Notify(method string, params json.RawMessage) error {
	msg := &Message{JSONRPC: jsonRPCVersion, Method: method, Params: params}
	return c.send(msg)
}

// Call sends a request and returns a channel on which the response will
// be delivered. The channel is buffered (size 1) so the read loop never
// blocks delivering to it, and is closed after exactly one send: either
// the correlated response, or (if the connection closes/errors first) a
// synthetic error Message.
//
// The send itself is synchronous, so ordering relative to Notify/Reply
// calls made from the same goroutine is preserved; it is the caller's
// choice whether to block on the returned channel inline or hand the
// wait off to a goroutine.
func (c *Conn) Call(method string, params json.RawMessage) (<-chan *Message, error) {
	id := NewIntID(c.seq.Add(1))
	ch := make(chan *Message, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("jsonrpc: call %s: connection closed", method)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	msg := &Message{JSONRPC: jsonRPCVersion, ID: &id, Method: method, Params: params}
	if err := c.send(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("jsonrpc: call %s: %w", method, err)
	}
	return ch, nil
}

// Reply sends a response to a request identified by id. Exactly one of
// result or e should be meaningful; if e is non-nil this is an error
// response. If e is nil and result is nil, the response still carries an
// explicit JSON "result": null (per JSON-RPC 2.0, a response must carry
// either result or error, and "result missing" is not the same as
// "result is null").
func (c *Conn) Reply(id ID, result json.RawMessage, e *Error) error {
	msg := &Message{JSONRPC: jsonRPCVersion, ID: &id}
	if e != nil {
		msg.Error = e
	} else {
		if result == nil {
			result = nullResult
		}
		msg.Result = result
	}
	return c.send(msg)
}

// Done returns a channel that is closed once Run has returned.
func (c *Conn) Done() <-chan struct{} {
	return c.done
}

// Close closes the underlying transport. This will typically cause Run's
// blocked read to fail, ending the read loop and releasing pending
// calls.
func (c *Conn) Close() error {
	if err := c.rwc.Close(); err != nil {
		return fmt.Errorf("jsonrpc: close: %w", err)
	}
	return nil
}

func (c *Conn) send(m *Message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("jsonrpc: marshal message: %w", err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := writeFrame(c.rwc, &c.writeBuf, data); err != nil {
		return fmt.Errorf("jsonrpc: write: %w", err)
	}
	return nil
}

// resolvePending delivers a response message to its matching Call
// channel, if any pending call is waiting on that id. Responses with no
// matching pending call (unexpected/duplicate/late) are dropped.
func (c *Conn) resolvePending(m *Message) {
	if m.ID == nil {
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[*m.ID]
	if ok {
		delete(c.pending, *m.ID)
	}
	c.mu.Unlock()

	if ok {
		ch <- m
		close(ch)
	}
}

// releasePending fails every call still awaiting a response when the
// read loop exits: each pending channel receives one synthetic error
// Message (code CodeInternalError) and is then closed, so a caller
// blocked on `<-ch` is always unblocked rather than left hanging.
func (c *Conn) releasePending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		ch <- &Message{
			JSONRPC: jsonRPCVersion,
			Error: &Error{
				Code:    CodeInternalError,
				Message: "jsonrpc: connection closed before response was received",
			},
		}
		close(ch)
		delete(c.pending, id)
	}
	c.closed = true
}
