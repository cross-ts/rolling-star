package gateway

import (
	"context"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

func (d *Downstream) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if d.sess == nil {
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: "rolling-star: no downstream handler configured for " + m.Method,
			})
		}

		return
	}

	s := d.sess
	if handler, ok := s.serverHandlers[m.Method]; ok && m.IsRequest() {
		result, errObj := handler(ctx, d, m.Params)
		_ = c.Reply(*m.ID, result, errObj)
		return
	}

	s.relay(c, s.upstream, m, "client")
}
