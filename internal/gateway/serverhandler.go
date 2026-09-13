package gateway

import (
	"context"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// Handle implements jsonrpc.Handler for a Downstream's connection, i.e.
// the server -> client direction: everything a downstream sends us that
// isn't a response to something we called.
//
// The one structural concession to the deferred handler-table work
// (actions/readFile, a locally-answered workspace/configuration, gh
// CLI-resolved initializationOptions, ...) is the branch at the top: in
// v1, Session.serverHandlers is always empty, so it never fires, but
// adding entries to that map later must not require touching anything
// below this branch (docs/init.md §3).
//
// Everything else is relayed upstream verbatim via Session.relay:
// publishDiagnostics, logMessage, showMessage, registerCapability,
// workspace/configuration, applyEdit, and any custom method (e.g.
// actions/readFile) all take the same path.
//
// Known v1 limitation: if a downstream issues a request (most plausibly
// workspace/configuration) while its own "initialize" call is still
// outstanding, we still relay it upstream here -- but the upstream client
// is guaranteed to still be blocked on our own "initialize" response at
// that point (see lifecycle.go's handleInitialize, which is fully
// synchronous), so a spec-conformant client cannot answer until we
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
func (d *Downstream) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if d.sess == nil {
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: "rolling-star: no downstream handler configured for " + m.Method,
			})
		}
		// Notifications with nowhere to go are dropped.
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
