package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/languageserver"
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

func (g *Gateway) route(c endpoint, m *jsonrpc.Message) {
	switch m.Method {
	case "textDocument/didOpen":
		g.handleDidOpen(m)
		return
	case "textDocument/didClose":
		g.handleDidClose(m)
		return
	case "$/cancelRequest":

		return
	}
	if broadcastMethods[m.Method] {
		g.broadcastNotification(m)
		return
	}

	if uri, ok := docURI(m.Params); ok {
		g.handleDocumentMessage(c, m, uri)
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

func (g *Gateway) handleDidOpen(m *jsonrpc.Message) {
	var p struct {
		TextDocument struct {
			URI        string `json:"uri"`
			LanguageID string `json:"languageId"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		g.log.Error("didOpen: failed to decode params", "error", err)
		return
	}
	if p.TextDocument.URI == "" {
		g.log.Error("didOpen: missing textDocument.uri")
		return
	}

	server := g.routeAndBind(p.TextDocument.URI, p.TextDocument.LanguageID, true)
	if server != nil {
		relay(g.log, g.client, server, m, server.Name())
	}
}

func (g *Gateway) handleDidClose(m *jsonrpc.Message) {
	uri, ok := docURI(m.Params)
	if !ok {
		g.log.Error("didClose: missing uri")
		return
	}

	server, _ := g.lookupBinding(uri)
	if server != nil {
		relay(g.log, g.client, server, m, server.Name())
	}

	g.mu.Lock()
	delete(g.documentServers, uri)
	g.mu.Unlock()
}

func (g *Gateway) handleDocumentMessage(c endpoint, m *jsonrpc.Message, uri string) {
	server, known := g.lookupBinding(uri)
	if !known {
		server = g.routeAndBind(uri, "", false)
	}
	if server == nil {
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, nil)
		}
		return
	}
	relay(g.log, c, server, m, server.Name())
}

func (g *Gateway) routeAndBind(uri, languageID string, warnOnMiss bool) *languageserver.Server {
	g.mu.Lock()
	rootPath := g.rootPath
	servers := g.languageServers
	g.mu.Unlock()

	path := router.PathForRouting(rootPath, uri)
	serverName, matched := g.router.Route(languageID, path)

	var server *languageserver.Server
	if matched {
		if i := slices.IndexFunc(servers, func(cand *languageserver.Server) bool { return cand.Name() == serverName }); i >= 0 {
			server = servers[i]
		}
	}

	g.mu.Lock()
	g.documentServers[uri] = server
	g.mu.Unlock()

	switch {
	case server != nil:
		g.log.Info("routed document to language server",
			"uri", uri, "languageId", languageID, "path", path, "server", server.Name())
	case matched && warnOnMiss:

		g.log.Warn("document matched a routing rule, but its server is not running; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path, "server", serverName)
	case warnOnMiss:
		g.log.Warn("no language server matched document; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path)
	}
	return server
}

func (g *Gateway) lookupBinding(uri string) (server *languageserver.Server, known bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	server, known = g.documentServers[uri]
	return server, known
}

func (g *Gateway) broadcastNotification(m *jsonrpc.Message) {
	for _, server := range g.snapshotLanguageServers() {
		if err := server.Notify(m.Method, m.Params); err != nil {
			g.log.Error("broadcast notification failed", "server", server.Name(), "method", m.Method, "error", err)
		}
	}
}

func relay(log *slog.Logger, from, to endpoint, m *jsonrpc.Message, peer string) {
	if !m.IsRequest() {
		if err := to.Notify(m.Method, m.Params); err != nil {
			if log != nil {
				log.Error("relay notification failed", "peer", peer, "method", m.Method, "error", err)
			}
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
