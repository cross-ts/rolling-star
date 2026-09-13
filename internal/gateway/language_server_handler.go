package gateway

import (
	"context"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

func (s *LanguageServer) Handle(_ context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if s.upstream == nil {
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeInternalError,
				Message: "rolling-star: language server is not attached to a gateway",
			})
		}

		return
	}

	relay(s.log, c, s.upstream, m, "client")
}
