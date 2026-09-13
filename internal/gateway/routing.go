package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/cross-ts/rolling-star/internal/jsonrpc"
	"github.com/cross-ts/rolling-star/internal/lsp"
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

func (g *Gateway) route(m *jsonrpc.Message) {
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
		g.handleDocumentMessage(m, uri)
		return
	}

	if strings.HasPrefix(m.Method, "$/") {

		return
	}

	if m.IsRequest() {

		_ = g.client.Reply(*m.ID, nil, &jsonrpc.Error{
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
		slog.Error("didOpen: failed to decode params", "error", err)
		return
	}
	if p.TextDocument.URI == "" {
		slog.Error("didOpen: missing textDocument.uri")
		return
	}

	server := g.routeAndBind(p.TextDocument.URI, p.TextDocument.LanguageID, true)
	if server != nil {
		if err := server.Notify(m.Method, m.Params); err != nil {
			slog.Error("failed to forward client notification", "server", server.Name(), "method", m.Method, "error", err)
		}
	}
}

func (g *Gateway) handleDidClose(m *jsonrpc.Message) {
	uri, ok := docURI(m.Params)
	if !ok {
		slog.Error("didClose: missing uri")
		return
	}

	server, _ := g.lookupBinding(uri)
	if server != nil {
		if err := server.Notify(m.Method, m.Params); err != nil {
			slog.Error("failed to forward client notification", "server", server.Name(), "method", m.Method, "error", err)
		}
	}

	g.mu.Lock()
	delete(g.documentServers, uri)
	g.mu.Unlock()
}

func (g *Gateway) handleDocumentMessage(m *jsonrpc.Message, uri string) {
	server, known := g.lookupBinding(uri)
	if !known {
		server = g.routeAndBind(uri, "", false)
	}
	if server == nil {
		if m.IsRequest() {
			_ = g.client.Reply(*m.ID, nil, nil)
		}
		return
	}

	if !m.IsRequest() {
		if err := server.Notify(m.Method, m.Params); err != nil {
			slog.Error("failed to forward client notification", "server", server.Name(), "method", m.Method, "error", err)
		}
		return
	}

	ch, err := server.Call(m.Method, m.Params)
	if err != nil {
		_ = g.client.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("rolling-star: failed to forward %s to %s: %v", m.Method, server.Name(), err),
		})
		return
	}

	id := *m.ID
	go func() {
		msg := <-ch
		_ = g.client.Reply(id, msg.Result, msg.Error)
	}()
}

func (g *Gateway) routeAndBind(uri, languageID string, warnOnMiss bool) *lsp.Server {
	g.mu.Lock()
	rootPath := g.rootPath
	servers := g.languageServers
	g.mu.Unlock()

	path := router.PathForRouting(rootPath, uri)
	serverName, matched := g.router.Route(languageID, path)

	var server *lsp.Server
	if matched {
		if i := slices.IndexFunc(servers, func(cand *lsp.Server) bool { return cand.Name() == serverName }); i >= 0 {
			server = servers[i]
		}
	}

	g.mu.Lock()
	g.documentServers[uri] = server
	g.mu.Unlock()

	switch {
	case server != nil:
		slog.Info("routed document to language server",
			"uri", uri, "languageId", languageID, "path", path, "server", server.Name())
	case matched && warnOnMiss:

		slog.Warn("document matched a routing rule, but its server is not running; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path, "server", serverName)
	case warnOnMiss:
		slog.Warn("no language server matched document; further messages for it will be dropped",
			"uri", uri, "languageId", languageID, "path", path)
	}
	return server
}

func (g *Gateway) lookupBinding(uri string) (server *lsp.Server, known bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	server, known = g.documentServers[uri]
	return server, known
}

func (g *Gateway) broadcastNotification(m *jsonrpc.Message) {
	for _, server := range g.snapshotLanguageServers() {
		if err := server.Notify(m.Method, m.Params); err != nil {
			slog.Error("broadcast notification failed", "server", server.Name(), "method", m.Method, "error", err)
		}
	}
}
