package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/router"
)

// docParams is the one small shape that covers every textDocument/*
// method's URI, in both forms LSP uses: {"textDocument":{"uri":...}}
// (the overwhelming majority: hover, definition, didChange, didClose,
// ...) and {"uri":...} directly (a handful of notifications). A
// per-method table would just be this same fact restated N times.
type docParams struct {
	TextDocument *struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	URI *string `json:"uri"`
}

// docURI extracts the document URI from a message's params, if any.
func docURI(params json.RawMessage) (string, bool) {
	if len(params) == 0 {
		return "", false
	}
	var p docParams
	if err := json.Unmarshal(params, &p); err != nil {
		return "", false
	}
	if p.TextDocument != nil && p.TextDocument.URI != "" {
		return p.TextDocument.URI, true
	}
	if p.URI != nil && *p.URI != "" {
		return *p.URI, true
	}
	return "", false
}

// broadcastMethods are notifications with no single document to route on
// but that every downstream still needs to see (§4.4).
var broadcastMethods = map[string]bool{
	"workspace/didChangeConfiguration":    true,
	"workspace/didChangeWatchedFiles":     true,
	"workspace/didChangeWorkspaceFolders": true,
	"$/setTrace":                          true,
}

// route implements the client -> server dispatch policy for everything
// that isn't a lifecycle method (those are handled directly in
// Session.Handle): document-bound traffic is routed to the server bound
// to that document's URI, and the handful of document-context-less cases
// from §4.4 are handled explicitly. It is invoked synchronously from the
// upstream Conn's read loop, so message ordering as sent by the client is
// preserved end to end (see forwardNotification/forwardRequest below for
// how that invariant survives being combined with request/response
// correlation).
func (s *Session) route(c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "textDocument/didOpen":
		s.handleDidOpen(m)
		return
	case "textDocument/didClose":
		s.handleDidClose(m)
		return
	case "$/cancelRequest":
		// The id it refers to is the upstream's; we deliberately keep no
		// id table to remap it against, so there is nothing correct to
		// do but drop it.
		return
	}
	if broadcastMethods[m.Method] {
		s.broadcastNotification(m)
		return
	}

	if uri, ok := docURI(m.Params); ok {
		s.handleDocumentMessage(c, m, uri)
		return
	}

	if strings.HasPrefix(m.Method, "$/") {
		// Any other $/* traffic (progress, telemetry, ...) is dropped:
		// these are LSP-optional notifications with no natural single
		// downstream to route to.
		return
	}

	if m.IsRequest() {
		// This is consistent, not just convenient: T5's mergeCapabilities
		// already strips workspaceSymbolProvider, executeCommandProvider,
		// and workspace from what we advertise to the client, so a
		// well-behaved client never sends workspace/symbol,
		// workspace/executeCommand, or similar workspace-scoped requests
		// in the first place. We never promised to route them.
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeMethodNotFound,
			Message: fmt.Sprintf("rolling-star: no document context and no route for %s", m.Method),
		})
	}
	// A notification with no document context and no known broadcast
	// method: nothing sane to do with it, drop.
}

// handleDidOpen decodes the opened document's uri and languageId, routes
// it, binds the result (possibly nil) in s.docs, and forwards the
// notification to the bound server, if any.
func (s *Session) handleDidOpen(m *jsonrpc.Message) {
	var p struct {
		TextDocument struct {
			URI        string `json:"uri"`
			LanguageID string `json:"languageId"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		s.log.Error("didOpen: failed to decode params", "error", err)
		return
	}
	if p.TextDocument.URI == "" {
		s.log.Error("didOpen: missing textDocument.uri")
		return
	}

	d := s.routeAndBind(p.TextDocument.URI, p.TextDocument.LanguageID, true)
	s.forwardNotification(d, m)
}

// handleDidClose forwards the notification to whatever server the
// document was bound to (if any), then removes the binding: the document
// no longer exists as far as rolling-star is concerned.
func (s *Session) handleDidClose(m *jsonrpc.Message) {
	uri, ok := docURI(m.Params)
	if !ok {
		s.log.Error("didClose: missing uri")
		return
	}

	d, _ := s.lookupBinding(uri)
	s.forwardNotification(d, m)

	s.mu.Lock()
	delete(s.docs, uri)
	s.mu.Unlock()
}

// handleDocumentMessage handles any message other than didOpen/didClose
// that carries a document uri: it looks up the existing binding, or -- on
// a miss, defensively -- routes on the fly with languageID=="" and caches
// the result (some clients issue requests before didOpen). If the
// document is unroutable (bound to nil, whether from didOpen or from this
// on-the-fly routing), the message is dropped silently: there is no
// default server, and this is required for §4.3's "every later message
// for that URI is dropped silently" to hold consistently regardless of
// how the nil binding was reached.
func (s *Session) handleDocumentMessage(c *jsonrpc.Conn, m *jsonrpc.Message, uri string) {
	d, known := s.lookupBinding(uri)
	if !known {
		d = s.routeAndBind(uri, "", false)
	}
	if d == nil {
		return
	}
	s.forward(c, m, d)
}

// routeAndBind computes the routing path for uri under the session's
// root, matches it against the router, resolves the matched server name
// to a currently-running *Downstream, stores the result (possibly nil)
// in s.docs, and returns it. If warnOnMiss is set and nothing matched,
// a single warning is logged for this uri.
func (s *Session) routeAndBind(uri, languageID string, warnOnMiss bool) *Downstream {
	s.mu.Lock()
	rootPath := s.rootPath
	servers := s.servers
	s.mu.Unlock()

	path := router.PathForRouting(rootPath, uri)
	serverName, ok := s.router.Route(languageID, path)

	var d *Downstream
	if ok {
		for _, cand := range servers {
			if cand.Def.Name == serverName {
				d = cand
				break
			}
		}
		// A rule can name a server that isn't currently running (it
		// failed to start/initialize and was dropped in T5's
		// handleInitialize); treat that the same as no match.
		ok = ok && d != nil
	}

	s.mu.Lock()
	s.docs[uri] = d
	s.mu.Unlock()

	if !ok && warnOnMiss {
		s.log.Warn("no downstream server matched document; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path)
	}
	return d
}

// lookupBinding reports the server bound to uri and whether that binding
// is known at all (as opposed to never having been routed).
func (s *Session) lookupBinding(uri string) (d *Downstream, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, known = s.docs[uri]
	return d, known
}

// broadcastNotification sends m, verbatim, to every currently-running
// downstream server.
func (s *Session) broadcastNotification(m *jsonrpc.Message) {
	for _, d := range s.snapshotServers() {
		if err := d.Conn().Notify(m.Method, m.Params); err != nil {
			s.log.Error("broadcast notification failed", "server", d.Def.Name, "method", m.Method, "error", err)
		}
	}
}

// forward sends m to d, as a notification or a request depending on m's
// own shape.
func (s *Session) forward(c *jsonrpc.Conn, m *jsonrpc.Message, d *Downstream) {
	if m.IsRequest() {
		s.forwardRequest(c, m, d)
		return
	}
	s.forwardNotification(d, m)
}

// forwardNotification relays m to d inline, from the read loop, so
// ordering relative to other forwarded traffic is exactly what the
// client sent. d may be nil (an unroutable document), in which case this
// is a no-op.
func (s *Session) forwardNotification(d *Downstream, m *jsonrpc.Message) {
	if d == nil {
		return
	}
	if err := d.Conn().Notify(m.Method, m.Params); err != nil {
		s.log.Error("forward notification failed", "server", d.Def.Name, "method", m.Method, "error", err)
	}
}

// forwardRequest relays a client request to d. The send itself
// (conn.Call) happens inline, from the read loop, so it is ordered
// exactly like forwardNotification with respect to other traffic sent to
// d; only waiting for the response is handed to a goroutine, so the
// upstream read loop is never blocked on a downstream's answer.
//
// The upstream request id is captured by the goroutine's closure; d's
// Conn.Call allocates its own fresh id for the downstream leg. That pair
// of ids is the entire "remap": no separate id table is kept. The
// downstream's error object (code/message/data) is copied verbatim into
// the upstream reply so it round-trips exactly.
func (s *Session) forwardRequest(c *jsonrpc.Conn, m *jsonrpc.Message, d *Downstream) {
	ch, err := d.Conn().Call(m.Method, m.Params)
	if err != nil {
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to forward %s to %s: %v", m.Method, d.Def.Name, err),
		})
		return
	}

	upstreamID := *m.ID
	go func() {
		msg := <-ch
		_ = c.Reply(upstreamID, msg.Result, msg.Error)
	}()
}
