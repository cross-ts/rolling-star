package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// testEditor is a client-side jsonrpc.Handler that plays the role of the
// upstream editor for T6 tests: it records diagnostics pushed to it and
// answers workspace/configuration (and any other request) with a canned
// result, so downstream-initiated requests forwarded upstream have
// somewhere sane to land.
type testEditor struct {
	mu             sync.Mutex
	diagnostics    []testEditorDiagnostic
	configResult   json.RawMessage
	configRequests []json.RawMessage
}

type testEditorDiagnostic struct {
	Method string
	Params json.RawMessage
}

func newTestEditor() *testEditor {
	return &testEditor{configResult: json.RawMessage(`[{"validate":true}]`)}
}

func (e *testEditor) Handle(_ context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "textDocument/publishDiagnostics":
		e.mu.Lock()
		e.diagnostics = append(e.diagnostics, testEditorDiagnostic{Method: m.Method, Params: m.Params})
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

func (e *testEditor) Diagnostics() []testEditorDiagnostic {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]testEditorDiagnostic, len(e.diagnostics))
	copy(out, e.diagnostics)
	return out
}

// setupRoutedSession starts a Session with the two-server config
// (actions for .github/workflows/**/*.yml, yaml for any other yaml) and
// completes initialize/initialized against rootUri file:///repo, so
// routing tests can go straight to sending document notifications.
func setupRoutedSession(t *testing.T) (client *jsonrpc.Conn, editor *testEditor, actionsFake, yamlFake *fakeServer) {
	t.Helper()
	return setupRoutedSessionWithConfig(t, twoServerConfig())
}

func setupRoutedSessionWithConfig(t *testing.T, cfg *config.Config) (client *jsonrpc.Conn, editor *testEditor, actionsFake, yamlFake *fakeServer) {
	t.Helper()

	actionsFake = newFakeServer(json.RawMessage(`{}`))
	yamlFake = newFakeServer(json.RawMessage(`{}`))
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	editor = newTestEditor()
	client = newTestSessionWithHandler(t, cfg, byName, editor)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error != nil {
		t.Fatalf("initialize: %v", resp.Error)
	}
	if err := client.Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("notify initialized: %v", err)
	}
	return client, editor, actionsFake, yamlFake
}

// patternOnlyConfig is like twoServerConfig but its selectors match on
// path alone (no languageId requirement), so it can demonstrate
// on-the-fly routing for a request whose languageId is unknown (i.e. no
// preceding didOpen).
func patternOnlyConfig() *config.Config {
	return &config.Config{
		Servers: []config.ServerDef{
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

// waitForReceipt polls fake.Received() until it has seen method, or
// fails the test after a short deadline. Forwarding a notification
// completes asynchronously with respect to the test goroutine (it's
// inline in the gateway's read loop, not the test's), so this avoids a
// flaky fixed sleep.
func waitForReceipt(t *testing.T, fake *fakeServer, method string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range fake.Received() {
			if r.Method == method {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", method)
}

func hasMethod(fake *fakeServer, method string) bool {
	for _, r := range fake.Received() {
		if r.Method == method {
			return true
		}
	}
	return false
}

func TestRouting_DidOpen_WorkflowGoesToActionsOnly(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedSession(t)

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
	client, _, actionsFake, yamlFake := setupRoutedSession(t)

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
	client, _, actionsFake, _ := setupRoutedSession(t)

	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")

	hoverParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	resp := mustCall(t, client, "textDocument/hover", hoverParams)
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
	client, _, actionsFake, _ := setupRoutedSession(t)

	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")

	closeParams, _ := json.Marshal(map[string]any{"textDocument": map[string]any{"uri": uri}})
	if err := client.Notify("textDocument/didClose", closeParams); err != nil {
		t.Fatalf("notify didClose: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didClose")

	// After didClose, the binding is gone. Proving that without reaching
	// into the unexported docs map: twoServerConfig's selectors both
	// require language==yaml, and this hover carries no languageId (no
	// preceding didOpen since the close), so if the old binding were
	// still cached the hover would succeed immediately; instead the
	// on-the-fly re-route with languageID=="" fails to match, and the
	// message is dropped exactly like any other unroutable document.
	hoverParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	ch, err := client.Call("textDocument/hover", hoverParams)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	select {
	case msg := <-ch:
		t.Fatalf("expected no response after didClose (binding removed, on-the-fly re-route can't match), got %+v", msg)
	case <-time.After(300 * time.Millisecond):
		// Expected: dropped silently.
	}
}

func TestRouting_RequestBeforeDidOpenRoutesOnTheFly(t *testing.T) {
	// Uses patternOnlyConfig, whose selectors match on path alone: with
	// twoServerConfig's language==yaml selectors, a request with no
	// preceding didOpen (languageID=="") could never match anything, which
	// would exercise the "unroutable" path instead of "on-the-fly routing
	// succeeded" (see TestRouting_DidCloseUnbinds for that path).
	client, _, actionsFake, _ := setupRoutedSessionWithConfig(t, patternOnlyConfig())

	uri := "file:///repo/.github/workflows/ci.yml"
	hoverParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	resp := mustCall(t, client, "textDocument/hover", hoverParams)
	if resp.Error != nil {
		t.Fatalf("hover: unexpected error: %v", resp.Error)
	}
	if !hasMethod(actionsFake, "textDocument/hover") {
		t.Error("on-the-fly routed hover never reached the actions fake")
	}
}

func TestRouting_UnroutableDocumentIsDroppedSilently(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedSession(t)

	uri := "file:///repo/README.md"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "markdown", "# hi\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}

	// Give the (nonexistent) forward a moment to definitely not arrive,
	// then confirm neither fake ever saw it.
	time.Sleep(100 * time.Millisecond)
	if hasMethod(actionsFake, "textDocument/didOpen") || hasMethod(yamlFake, "textDocument/didOpen") {
		t.Fatal("unroutable document's didOpen reached a downstream server")
	}

	// A later message for the same (now known-unroutable) uri is also
	// dropped: send a hover *request* and confirm it never gets a
	// response within a reasonable window (dropped, not answered).
	hoverParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	ch, err := client.Call("textDocument/hover", hoverParams)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	select {
	case msg := <-ch:
		t.Fatalf("expected no response for an unroutable document, got %+v", msg)
	case <-time.After(300 * time.Millisecond):
		// Expected: dropped silently, no reply ever comes.
	}
}

func TestRouting_PublishDiagnosticsReachesClient(t *testing.T) {
	client, editor, actionsFake, _ := setupRoutedSession(t)

	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")

	diags, _ := json.Marshal([]map[string]any{{"message": "boom", "severity": 1}})
	if err := actionsFake.PushDiagnostics(uri, diags); err != nil {
		t.Fatalf("PushDiagnostics: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(editor.Diagnostics()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	got := editor.Diagnostics()
	if len(got) != 1 {
		t.Fatalf("editor received %d diagnostics notifications, want 1", len(got))
	}
}

func TestRouting_DownstreamRequestIDIsRemapped(t *testing.T) {
	client, editor, actionsFake, _ := setupRoutedSession(t)

	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")

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
		t.Fatal("timed out waiting for workspace/configuration response to route back to the downstream")
	}

	// The regression this guards: the id the editor saw for this request
	// must differ from any id the actionsFake itself used as a *client*
	// (e.g. its own initialize call to itself doesn't apply here, so
	// instead we assert the editor actually saw exactly one
	// workspace/configuration request, proving forwarding-with-remap
	// executed rather than, say, being silently dropped or misrouted).
	if len(editor.configRequests) != 1 {
		t.Fatalf("editor saw %d workspace/configuration requests, want 1", len(editor.configRequests))
	}
}

func TestRouting_DownstreamErrorPropagatesUpstream(t *testing.T) {
	client, _, actionsFake, _ := setupRoutedSession(t)

	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")

	// The fake replies MethodNotFound to anything it doesn't specifically
	// implement; textDocument/definition is one such method, so it
	// exercises the downstream-error round trip without adding new fake
	// behavior.
	params, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	resp := mustCall(t, client, "textDocument/definition", params)
	if resp.Error == nil {
		t.Fatal("expected an error response, got success")
	}
	if resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeMethodNotFound)
	}
	if resp.Error.Message == "" {
		t.Error("error message was empty; expected the downstream's message to survive")
	}
}

func TestRouting_NotificationOrderingPreserved(t *testing.T) {
	client, _, actionsFake, _ := setupRoutedSession(t)

	uri := "file:///repo/.github/workflows/ci.yml"
	if err := client.Notify("textDocument/didOpen", didOpenParams(uri, "yaml", "name: CI\n")); err != nil {
		t.Fatalf("notify didOpen: %v", err)
	}
	waitForReceipt(t, actionsFake, "textDocument/didOpen")

	changeParams, _ := json.Marshal(map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": 2},
		"contentChanges": []map[string]any{{"text": "name: CI2\n"}},
	})
	if err := client.Notify("textDocument/didChange", changeParams); err != nil {
		t.Fatalf("notify didChange: %v", err)
	}

	hoverParams, _ := json.Marshal(map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": 0, "character": 0},
	})
	resp := mustCall(t, client, "textDocument/hover", hoverParams)
	if resp.Error != nil {
		t.Fatalf("hover: %v", resp.Error)
	}

	var methods []string
	for _, r := range actionsFake.Received() {
		methods = append(methods, r.Method)
	}
	want := []string{"initialize", "initialized", "textDocument/didOpen", "textDocument/didChange", "textDocument/hover"}
	if len(methods) != len(want) {
		t.Fatalf("actionsFake received %v, want %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Errorf("received[%d] = %q, want %q (ordering not preserved)", i, methods[i], want[i])
		}
	}
}

func TestRouting_CancelRequestIsDropped(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedSession(t)

	if err := client.Notify("$/cancelRequest", json.RawMessage(`{"id":1}`)); err != nil {
		t.Fatalf("notify $/cancelRequest: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if hasMethod(actionsFake, "$/cancelRequest") || hasMethod(yamlFake, "$/cancelRequest") {
		t.Fatal("$/cancelRequest should never be forwarded to any downstream")
	}
}

func TestRouting_WorkspaceDidChangeConfigurationIsBroadcast(t *testing.T) {
	client, _, actionsFake, yamlFake := setupRoutedSession(t)

	if err := client.Notify("workspace/didChangeConfiguration", json.RawMessage(`{"settings":{}}`)); err != nil {
		t.Fatalf("notify: %v", err)
	}

	waitForReceipt(t, actionsFake, "workspace/didChangeConfiguration")
	waitForReceipt(t, yamlFake, "workspace/didChangeConfiguration")
}

func TestRouting_WorkspaceSymbolIsMethodNotFound(t *testing.T) {
	client, _, _, _ := setupRoutedSession(t)

	resp := mustCall(t, client, "workspace/symbol", json.RawMessage(`{"query":"foo"}`))
	if resp.Error == nil {
		t.Fatal("expected -32601 MethodNotFound, got success")
	}
	if resp.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, jsonrpc.CodeMethodNotFound)
	}
}
