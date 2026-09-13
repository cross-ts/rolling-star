package gateway

import (
	"context"
	"fmt"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// downstreamHandler is the jsonrpc.Handler for one Downstream's
// connection, i.e. the server -> client direction: everything a
// downstream sends us that isn't a response to something we called.
// It is assigned to Downstream.handler right after StartDownstream
// returns (see lifecycle.go's handleInitialize), which is the "accept a
// jsonrpc.Handler at construction" seam T4 built for exactly this.
type downstreamHandler struct {
	s *Session
	d *Downstream
}

// Handle implements jsonrpc.Handler.
//
// The one structural concession to the deferred handler-table work
// (actions/readFile, a locally-answered workspace/configuration, gh
// CLI-resolved initializationOptions, ...) is the branch at the top: in
// v1, Session.serverHandlers is always empty, so it never fires, but
// adding entries to that map later must not require touching anything
// below this branch (docs/init.md §3).
//
// Everything else is forwarded upstream verbatim: publishDiagnostics,
// logMessage, showMessage, registerCapability, workspace/configuration,
// applyEdit, and any custom method (e.g. actions/readFile) all take the
// same path. Requests use the same inline-Call-then-goroutine-wait
// pattern as the client->server direction (see forwardRequest in
// routing.go), just mirrored: the downstream's request id is captured by
// the goroutine's closure, and Session.upstream.Call allocates a fresh
// id for the upstream leg.
//
// Known v1 limitation: if a downstream issues a request (most plausibly
// workspace/configuration) while its own "initialize" call is still
// outstanding, we still forward it upstream here -- but the upstream
// client is guaranteed to still be blocked on our own "initialize"
// response at that point (see lifecycle.go's handleInitialize, which is
// fully synchronous), so a spec-conformant client cannot answer until we
// return, and a spec-conformant server does not send such a request
// before its own initialize response. Only a non-conformant peer on
// either side hits this, and when it does, it is no longer unbounded:
// lifecycle.go's handleInitialize bounds that server's Initialize call
// with downstreamInitializeTimeout, so a deadlocked downstream is
// eventually logged and dropped via the ordinary
// failed-to-initialize path, and the rest of the session still comes
// up. That is a bounded degradation, not a fix -- the correct fix is
// still answering such requests locally via serverHandlers instead of
// forwarding, which is deferred v1-scope work.
func (h *downstreamHandler) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if handler, ok := h.s.serverHandlers[m.Method]; ok && m.IsRequest() {
		result, errObj := handler(ctx, h.d, m.Params)
		_ = c.Reply(*m.ID, result, errObj)
		return
	}

	if m.IsRequest() {
		h.forwardRequestUpstream(c, m)
		return
	}
	h.forwardNotificationUpstream(m)
}

func (h *downstreamHandler) forwardNotificationUpstream(m *jsonrpc.Message) {
	if err := h.s.upstream.Notify(m.Method, m.Params); err != nil {
		h.s.log.Error("forward notification to upstream failed", "server", h.d.Def.Name, "method", m.Method, "error", err)
	}
}

func (h *downstreamHandler) forwardRequestUpstream(c *jsonrpc.Conn, m *jsonrpc.Message) {
	ch, err := h.s.upstream.Call(m.Method, m.Params)
	if err != nil {
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to forward %s from %s to client: %v", m.Method, h.d.Def.Name, err),
		})
		return
	}

	downstreamID := *m.ID
	go func() {
		msg := <-ch
		_ = c.Reply(downstreamID, msg.Result, msg.Error)
	}()
}
