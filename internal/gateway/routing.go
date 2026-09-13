package gateway

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/router"
)

type docParams struct {
	TextDocument *struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	URI *string `json:"uri"`
}

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

var broadcastMethods = map[string]bool{
	"workspace/didChangeConfiguration":    true,
	"workspace/didChangeWatchedFiles":     true,
	"workspace/didChangeWorkspaceFolders": true,
	"$/setTrace":                          true,
}

func (s *Session) route(c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "textDocument/didOpen":
		s.handleDidOpen(m)
		return
	case "textDocument/didClose":
		s.handleDidClose(m)
		return
	case "$/cancelRequest":

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

		return
	}

	if m.IsRequest() {

		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeMethodNotFound,
			Message: fmt.Sprintf("rolling-star: no document context and no route for %s", m.Method),
		})
	}

}

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

		s.log.Warn("document matched a routing rule, but its server is not running; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path, "server", serverName)
	case warnOnMiss:
		s.log.Warn("no downstream server matched document; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path)
	}
	return d
}

func (s *Session) lookupBinding(uri string) (d *Downstream, known bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, known = s.docs[uri]
	return d, known
}

func (s *Session) broadcastNotification(m *jsonrpc.Message) {
	for _, d := range s.snapshotServers() {
		if err := d.Conn().Notify(m.Method, m.Params); err != nil {
			s.log.Error("broadcast notification failed", "server", d.Def.Name, "method", m.Method, "error", err)
		}
	}
}

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
