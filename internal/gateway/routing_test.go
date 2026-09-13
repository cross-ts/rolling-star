package gateway

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type testEditor struct {
	mu             sync.Mutex
	diagnostics    []received
	configResult   json.RawMessage
	configRequests []json.RawMessage
}

func newTestEditor() *testEditor {
	return &testEditor{configResult: json.RawMessage(`[{"validate":true}]`)}
}

func (e *testEditor) Handle(_ context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "textDocument/publishDiagnostics":
		e.mu.Lock()
		e.diagnostics = append(e.diagnostics, received{Method: m.Method, Params: m.Params})
		e.mu.Unlock()
	case "workspace/configuration":
		e.mu.Lock()
		e.configRequests = append(e.configRequests, m.Params)
		e.mu.Unlock()
		_ = c.Reply(*m.ID, e.configResult, nil)
	default:
		if m.IsRequest() {
			_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: "testEditor: unhandled " + m.Method,
			})
		}
	}
}

func (e *testEditor) Diagnostics() []received {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.diagnostics)
}

func setupRoutedGateway(t *testing.T) (client *jsonrpc.Conn, editor *testEditor, actionsFake, yamlFake *fakeServer) {
	t.Helper()
	return setupRoutedGatewayWithConfig(t, twoServerConfig())
}

func setupRoutedGatewayWithConfig(t *testing.T, cfg *config.Config) (client *jsonrpc.Conn, editor *testEditor, actionsFake, yamlFake *fakeServer) {
	t.Helper()

	actionsFake = newFakeServer(json.RawMessage(`{}`))
	yamlFake = newFakeServer(json.RawMessage(`{}`))
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	editor = newTestEditor()
	client = newTestGatewayWithHandler(t, cfg, byName, editor)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error != nil {
		t.Fatalf("initialize: %v", resp.Error)
	}
	if err := client.Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("notify initialized: %v", err)
	}
	return client, editor, actionsFake, yamlFake
}

func patternOnlyConfig() *config.Config {
	return &config.Config{
		Servers: []config.LanguageServer{
			{
				Name:      "actions",
				Command:   "irrelevant",
				Selectors: []config.Selector{{Pattern: ".github/workflows/**/*.yml"}},
			},
			{
				Name:      "yaml",
				Command:   "irrelevant",
				Selectors: []config.Selector{{Pattern: "**/*.{yml,yaml}"}},
			},
		},
	}
}

func didOpenParams(uri, languageID, text string) json.RawMessage {
	p := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	}
	b, _ := json.Marshal(p)
	return b
}

func positionParams(uri string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	return b
}

func openWorkflowDoc(t *testing.T, client *jsonrpc.Conn, actionsFake *fakeServer) string {
	t.Helper()
	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")
	return uri
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitForReceipt(t *testing.T, fake *fakeServer, method string) {
	t.Helper()
	waitFor(t, func() bool { return hasMethod(fake, method) }, "timed out waiting for "+method)
}

func hasMethod(fake *fakeServer, method string) bool {
	return slices.ContainsFunc(fake.Received(), func(r received) bool { return r.Method == method })
}

func TestRouting_DidOpen_WorkflowGoesToActionsOnly(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedGateway(t)

	if err := client.Notify("textDocument/didOpen", didOpenParams(
		"file:///repo/.github/workflows/ci.yml", "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}

	waitForReceipt(t, actionsFake, "textDocument/didOpen")
	if hasMethod(yamlFake, "textDocument/didOpen") {
		t.Fatal("workflow didOpen reached the yaml fake; it should only reach actions")
	}
}

func TestRouting_DidOpen_PlainYAMLGoesToYAMLOnly(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedGateway(t)

	if err := client.Notify("textDocument/didOpen", didOpenParams(
		"file:///repo/docker-compose.yml", "yaml", "services: {}\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}

	waitForReceipt(t, yamlFake, "textDocument/didOpen")
	if hasMethod(actionsFake, "textDocument/didOpen") {
		t.Fatal("plain yaml didOpen reached the actions fake; it should only reach yaml")
	}
}

func TestRouting_HoverAfterDidOpen(t *testing.T) {
	client, _, actionsFake, _ := setupRoutedGateway(t)
	uri := openWorkflowDoc(t, client, actionsFake)

	resp := mustCall(t, client, "textDocument/hover", positionParams(uri))
	if resp.Error != nil {
		t.Fatalf("hover: unexpected error: %v", resp.Error)
	}
	if string(resp.Result) != `{"contents":"fake hover"}` {
		t.Errorf("hover result = %s, want the fake's canned hover", resp.Result)
	}
	if !hasMethod(actionsFake, "textDocument/hover") {
		t.Error("hover never reached the actions fake")
	}
}

func TestRouting_DidCloseUnbinds(t *testing.T) {
	client, _, actionsFake, _ := setupRoutedGateway(t)
	uri := openWorkflowDoc(t, client, actionsFake)

	closeParams, _ := json.Marshal(map[string]any{"textDocument": map[string]any{"uri": uri}})
	if err := client.Notify("textDocument/didClose", closeParams); err != nil {
		t.Fatalf("notify didClose: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didClose")

	resp := mustCall(t, client, "textDocument/hover", positionParams(uri))
	if resp.Error != nil {
		t.Fatalf("hover after didClose: unexpected error: %v", resp.Error)
	}
	if string(resp.Result) != "null" {
		t.Errorf("hover after didClose: result = %s, want null (binding removed, re-route can't match)", resp.Result)
	}
}

func TestRouting_RequestBeforeDidOpenRoutesOnTheFly(t *testing.T) {

	client, _, actionsFake, _ := setupRoutedGatewayWithConfig(t, patternOnlyConfig())

	uri := "file:///repo/.github/workflows/ci.yml"
	resp := mustCall(t, client, "textDocument/hover", positionParams(uri))
	if resp.Error != nil {
		t.Fatalf("hover: unexpected error: %v", resp.Error)
	}
	if !hasMethod(actionsFake, "textDocument/hover") {
		t.Error("on-the-fly routed hover never reached the actions fake")
	}
}

func TestRouting_UnroutableDocumentIsDroppedSilently(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedGateway(t)

	uri := "file:///repo/README.md"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "markdown", "# hi\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if hasMethod(actionsFake, "textDocument/didOpen") || hasMethod(yamlFake, "textDocument/didOpen") {
		t.Fatal("unroutable document's didOpen reached a language server")
	}

	resp := mustCall(t, client, "textDocument/hover", positionParams(uri))
	if resp.Error != nil {
		t.Fatalf("hover on unroutable document: unexpected error: %v", resp.Error)
	}
	if string(resp.Result) != "null" {
		t.Errorf("hover on unroutable document: result = %s, want null", resp.Result)
	}
}

func TestRouting_PublishDiagnosticsReachesClient(t *testing.T) {
	client, editor, actionsFake, _ := setupRoutedGateway(t)
	uri := openWorkflowDoc(t, client, actionsFake)

	diags, _ := json.Marshal([]map[string]any{{"message": "boom", "severity": 1}})
	if err := actionsFake.PushDiagnostics(uri, diags); err != nil {
		t.Fatalf("PushDiagnostics: %v", err)
	}

	waitFor(t, func() bool { return len(editor.Diagnostics()) > 0 }, "timed out waiting for diagnostics")
	got := editor.Diagnostics()
	if len(got) != 1 {
		t.Fatalf("editor received %d diagnostics notifications, want 1", len(got))
	}
}

func TestRouting_LanguageServerRequestIDIsRemapped(t *testing.T) {
	client, editor, actionsFake, _ := setupRoutedGateway(t)
	openWorkflowDoc(t, client, actionsFake)

	items, _ := json.Marshal([]map[string]any{{"section": "yaml"}})
	ch, err := actionsFake.AskConfiguration(items)
	if err != nil {
		t.Fatalf("AskConfiguration: %v", err)
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			t.Fatalf("workspace/configuration: unexpected error: %v", msg.Error)
		}
		if string(msg.Result) != `[{"validate":true}]` {
			t.Errorf("workspace/configuration result = %s, want the editor's canned result", msg.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for workspace/configuration response to route back to the language server")
	}

	if len(editor.configRequests) != 1 {
		t.Fatalf("editor saw %d workspace/configuration requests, want 1", len(editor.configRequests))
	}
}

func TestRouting_LanguageServerErrorPropagatesUpstream(t *testing.T) {
	client, _, actionsFake, _ := setupRoutedGateway(t)
	uri := openWorkflowDoc(t, client, actionsFake)

	resp := mustCall(t, client, "textDocument/definition", positionParams(uri))
	if resp.Error == nil {
		t.Fatal("expected an error response, got success")
	}
	if resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeMethodNotFound)
	}
	if resp.Error.Message == "" {
		t.Error("error message was empty; expected the language server's message to survive")
	}
}

func TestRouting_NotificationOrderingPreserved(t *testing.T) {
	client, _, actionsFake, _ := setupRoutedGateway(t)
	uri := openWorkflowDoc(t, client, actionsFake)

	changeParams, _ := json.Marshal(map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": "name: CI2\n"}},
	})
	if err := client.Notify("textDocument/didChange", changeParams); err != nil {
		t.Fatalf("notify didChange: %v", err)
	}

	resp := mustCall(t, client, "textDocument/hover", positionParams(uri))
	if resp.Error != nil {
		t.Fatalf("hover: %v", resp.Error)
	}

	assertMethods(t, actionsFake, "initialize", "initialized", "textDocument/didOpen", "textDocument/didChange", "textDocument/hover")
}

func TestRouting_CancelRequestIsDropped(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedGateway(t)

	if err := client.Notify("$/cancelRequest", json.RawMessage(`{"id":1}`)); err != nil {
		t.Fatalf("notify $/cancelRequest: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if hasMethod(actionsFake, "$/cancelRequest") || hasMethod(yamlFake, "$/cancelRequest") {
		t.Fatal("$/cancelRequest should never be forwarded to any language server")
	}
}

func TestRouting_WorkspaceDidChangeConfigurationIsBroadcast(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedGateway(t)

	if err := client.Notify("workspace/didChangeConfiguration", json.RawMessage(`{"settings":{}}`)); err != nil {
		t.Fatalf("notify: %v", err)
	}

	waitForReceipt(t, actionsFake, "workspace/didChangeConfiguration")
	waitForReceipt(t, yamlFake, "workspace/didChangeConfiguration")
}

func TestRouting_WorkspaceSymbolIsMethodNotFound(t *testing.T) {
	client, _, _, _ := setupRoutedGateway(t)

	resp := mustCall(t, client, "workspace/symbol", json.RawMessage(`{"query":"foo"}`))
	if resp.Error == nil {
		t.Fatal("expected -32601 MethodNotFound, got success")
	}
	if resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeMethodNotFound)
	}
}
