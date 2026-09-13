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

type Handler interface {
	Handle(ctx context.Context, c *Conn, m *Message)
}

type Conn struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader
	h   Handler

	readBuf []byte

	wmu      sync.Mutex
	writeBuf []byte

	seq atomic.Int64

	mu      sync.Mutex
	pending map[ID]chan *Message
	closed  bool

	done chan struct{}
}

func NewConn(rwc io.ReadWriteCloser, h Handler) *Conn {
	return &Conn{
		rwc:     rwc,
		r:       bufio.NewReader(rwc),
		h:       h,
		pending: make(map[ID]chan *Message),
		done:    make(chan struct{}),
	}
}

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

func (c *Conn) Notify(method string, params json.RawMessage) error {
	msg := &Message{JSONRPC: jsonRPCVersion, Method: method, Params: params}
	return c.send(msg)
}

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

func (c *Conn) Done() <-chan struct{} {
	return c.done
}

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
