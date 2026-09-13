package client

import (
	"context"
	"encoding/json"
	"io"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type Client struct {
	conn *jsonrpc.Conn
}

func New(transport io.ReadWriteCloser) *Client {
	return &Client{conn: jsonrpc.NewConn(transport)}
}

func (c *Client) Notify(method string, params json.RawMessage) error {
	return c.conn.Notify(method, params)
}

func (c *Client) Call(method string, params json.RawMessage) (<-chan *jsonrpc.Message, error) {
	return c.conn.Call(method, params)
}

func (c *Client) Reply(id jsonrpc.ID, result json.RawMessage, e *jsonrpc.Error) error {
	return c.conn.Reply(id, result, e)
}

func (c *Client) Run(ctx context.Context, handle func(*jsonrpc.Message)) error {
	done := make(chan error, 1)
	go func() { done <- c.conn.Run(ctx) }()

	for {
		select {
		case m, ok := <-c.conn.Messages():
			if !ok {
				return <-done
			}
			if handle != nil {
				handle(m)
			}
		case err := <-done:
			return err
		case <-ctx.Done():
			_ = c.conn.Close()
			return ctx.Err()
		}
	}
}
