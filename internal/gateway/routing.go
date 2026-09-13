package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
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
// preserved end to end (see relay below for how that invariant survives
// being combined with request/response correlation).
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
	if d != nil {
		s.relay(s.upstream, d.Conn(), m, d.Def.Name)
	}
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
	if d != nil {
		s.relay(s.upstream, d.Conn(), m, d.Def.Name)
	}

	s.mu.Lock()
	delete(s.docs, uri)
	s.mu.Unlock()
}

// handleDocumentMessage handles any message other than didOpen/didClose
// that carries a document uri: it looks up the existing binding, or -- on
// a miss, defensively -- routes on the fly with languageID=="" and caches
// the result (some clients issue requests before didOpen).
//
// If the document is unroutable (bound to nil, whether from didOpen or
// from this on-the-fly routing):
//   - a notification is dropped silently, per §4.3's "every later message
//     for that URI is dropped silently" -- there is no default server and
//     nothing sane to notify.
//   - a request gets a reply with a null result, NOT MethodNotFound and
//     NOT silence. Silence would leave the client blocked forever on that
//     request (a JSON-RPC request always gets exactly one response) --
//     that is a real, user-visible defect, not an acceptable consequence
//     of "no default server". MethodNotFound would be a lie: the merged
//     capabilities we advertised say the method exists, and the very same
//     method against a *routable* document in the same session works
//     fine. null is the LSP-conventional "no answer for this document"
//     shape for hover/definition/completion/etc., so that's what we send.
func (s *Session) handleDocumentMessage(c *jsonrpc.Conn, m *jsonrpc.Message, uri string) {
	d, known := s.lookupBinding(uri)
	if !known {
		d = s.routeAndBind(uri, "", false)
	}
	if d == nil {
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, nil)
		}
		return
	}
	s.relay(c, d.Conn(), m, d.Def.Name)
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
	serverName, matched := s.router.Route(languageID, path)

	var d *Downstream
	if matched {
		if i := slices.IndexFunc(servers, func(cand *Downstream) bool { return cand.Def.Name == serverName }); i >= 0 {
			d = servers[i]
		}
	}

	s.mu.Lock()
	s.docs[uri] = d
	s.mu.Unlock()

	switch {
	case d != nil:
		s.log.Info("routed document to downstream server",
			"uri", uri, "languageId", languageID, "path", path, "server", d.Def.Name)
	case matched && warnOnMiss:
		// A rule matched, but the server it named isn't currently
		// running (it failed to start/initialize and was dropped in
		// handleInitialize) -- distinct from no rule matching at all,
		// since this points at a startup failure, not a routing gap.
		s.log.Warn("document matched a routing rule, but its server is not running; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path, "server", serverName)
	case warnOnMiss:
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

// relay forwards m, received on from, to the peer connection to, in
// whichever of the two directions the caller needs -- client->server
// (from == s.upstream, to == some Downstream's Conn) or server->client
// (from == some Downstream's Conn, to == s.upstream). peer names the "to"
// side, for logging/error text only.
//
// A notification is sent to `to` inline, from the caller's read loop, so
// its ordering relative to other relayed traffic exactly matches what the
// sender emitted. A request's send is also inline for the same reason;
// only waiting for its response is handed to a goroutine, so `from`'s
// read loop is never blocked on `to`'s answer.
//
// The original request's id is captured by that goroutine's closure; to's
// Conn.Call allocates its own fresh id for the outgoing leg. That id pair
// is the entire "remap": no separate id table is kept anywhere. The
// response's error object (code/message/data) is copied verbatim into the
// reply on `from` so it round-trips exactly.
func (s *Session) relay(from, to *jsonrpc.Conn, m *jsonrpc.Message, peer string) {
	if !m.IsRequest() {
		if err := to.Notify(m.Method, m.Params); err != nil {
			s.log.Error("relay notification failed", "peer", peer, "method", m.Method, "error", err)
		}
		return
	}

	ch, err := to.Call(m.Method, m.Params)
	if err != nil {
		_ = from.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to forward %s to %s: %v", m.Method, peer, err),
		})
		return
	}

	id := *m.ID
	go func() {
		msg := <-ch
		_ = from.Reply(id, msg.Result, msg.Error)
	}()
}
