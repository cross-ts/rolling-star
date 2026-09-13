package client

import (
	"context"
	"io"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type Client struct {
	conn *jsonrpc.Conn
}

func New(transport io.ReadWriteCloser) *Client {
	return &Client{conn: jsonrpc.NewConn(transport)}
}

func (c *Client) Conn() *jsonrpc.Conn { return c.conn }

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
