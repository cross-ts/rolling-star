package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// noopHandler drops everything; it stands in for the test "editor" not
// expecting any server->client requests/notifications from the gateway
// in these lifecycle-only tests.
type noopHandler struct{}

func (noopHandler) Handle(context.Context, *jsonrpc.Conn, *jsonrpc.Message) {}

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// multiLauncher dispatches to a different fakeServer per server name, so
// a single Session (which only has one Launcher) can drive several
// distinct fakes.
func multiLauncher(byName map[string]*fakeServer) Launcher {
	return func(ctx context.Context, def config.ServerDef) (Process, error) {
		fs, ok := byName[def.Name]
		if !ok {
			fs = newFakeServer(json.RawMessage(`{}`))
		}
		return newFakeLauncher(fs)(ctx, def)
	}
}

// newTestSession wires a Session to launch fakes and starts Serve on one
// end of an in-memory pipe, returning a client-side jsonrpc.Conn playing
// the role of the upstream editor, with noopHandler standing in for the
// editor's own request/notification handling.
func newTestSession(t *testing.T, cfg *config.Config, byName map[string]*fakeServer) *jsonrpc.Conn {
	t.Helper()
	return newTestSessionWithHandler(t, cfg, byName, noopHandler{})
}

// newTestSessionWithHandler is like newTestSession but lets the caller
// supply the client-side handler, for tests that need the "editor" to
// answer downstream-initiated requests (e.g. workspace/configuration) or
// record notifications (e.g. publishDiagnostics) pushed to it.
func newTestSessionWithHandler(t *testing.T, cfg *config.Config, byName map[string]*fakeServer, clientHandler jsonrpc.Handler) *jsonrpc.Conn {
	t.Helper()

	sess, err := New(cfg, Options{Launcher: multiLauncher(byName), Logger: testLogger(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	clientSide, gatewaySide := net.Pipe()
	client := jsonrpc.NewConn(clientSide, clientHandler)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go client.Run(ctx)              //nolint:errcheck
	go sess.Serve(ctx, gatewaySide) //nolint:errcheck

	return client
}

func mustCall(t *testing.T, c *jsonrpc.Conn, method string, params json.RawMessage) *jsonrpc.Message {
	t.Helper()
	ch, err := c.Call(method, params)
	if err != nil {
		t.Fatalf("%s: call: %v", method, err)
	}
	select {
	case msg := <-ch:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: timed out waiting for response", method)
		return nil
	}
}

func twoServerConfig() *config.Config {
	return &config.Config{
		Servers: []config.ServerDef{
			{
				Name:                  "actions",
				Command:               "irrelevant",
				InitializationOptions: map[string]any{"actions": true},
				Selectors:             []config.Selector{{Language: "yaml", Pattern: ".github/workflows/**/*.yml"}},
			},
			{
				Name:      "yaml",
				Command:   "irrelevant",
				Selectors: []config.Selector{{Language: "yaml"}},
			},
		},
	}
}

func TestSession_InitializeFanOut(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{"hoverProvider":true}`))
	yamlFake := newFakeServer(json.RawMessage(`{"definitionProvider":true}`))
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestSession(t, twoServerConfig(), byName)

	initParams := json.RawMessage(`{
		"processId": 999999,
		"rootUri": "file:///repo",
		"capabilities": {"textDocument":{"hover":{}}},
		"clientInfo": {"name":"test-editor"}
	}`)
	resp := mustCall(t, client, "initialize", initParams)
	if resp.Error != nil {
		t.Fatalf("initialize: unexpected error: %v", resp.Error)
	}

	var result struct {
		Capabilities json.RawMessage `json:"capabilities"`
		ServerInfo   struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ServerInfo.Name != "rolling-star" {
		t.Errorf("serverInfo.name = %q, want rolling-star", result.ServerInfo.Name)
	}

	var caps map[string]json.RawMessage
	if err := json.Unmarshal(result.Capabilities, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if v, ok := asJSONBool(caps["hoverProvider"]); !ok || !v {
		t.Errorf("hoverProvider = %s, want true", caps["hoverProvider"])
	}
	if v, ok := asJSONBool(caps["definitionProvider"]); !ok || !v {
		t.Errorf("definitionProvider = %s, want true", caps["definitionProvider"])
	}
	if string(caps["textDocumentSync"]) != fixedTextDocumentSync {
		t.Errorf("textDocumentSync = %s, want %s", caps["textDocumentSync"], fixedTextDocumentSync)
	}

	// Each fake received exactly one initialize, with our own pid, the
	// server-specific initializationOptions (or none), and rootUri passed
	// through unchanged.
	for name, fake := range byName {
		recv := fake.Received()
		if len(recv) == 0 || recv[0].Method != "initialize" {
			t.Fatalf("%s: expected initialize as first message, got %+v", name, recv)
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(recv[0].Params, &params); err != nil {
			t.Fatalf("%s: decode initialize params: %v", name, err)
		}

		var pid int
		if err := json.Unmarshal(params["processId"], &pid); err != nil {
			t.Fatalf("%s: decode processId: %v", name, err)
		}
		if pid != os.Getpid() {
			t.Errorf("%s: processId = %d, want %d (our own pid)", name, pid, os.Getpid())
		}

		var rootURI string
		if err := json.Unmarshal(params["rootUri"], &rootURI); err != nil {
			t.Fatalf("%s: decode rootUri: %v", name, err)
		}
		if rootURI != "file:///repo" {
			t.Errorf("%s: rootUri = %q, want file:///repo (passthrough)", name, rootURI)
		}

		if capParam, ok := params["capabilities"]; !ok || string(capParam) != `{"textDocument":{"hover":{}}}` {
			t.Errorf("%s: capabilities = %s, want passthrough of client's capabilities", name, capParam)
		}
	}

	actionsParams := actionsFake.Received()[0].Params
	var actionsDecoded map[string]json.RawMessage
	_ = json.Unmarshal(actionsParams, &actionsDecoded)
	if string(actionsDecoded["initializationOptions"]) != `{"actions":true}` {
		t.Errorf("actions: initializationOptions = %s, want {\"actions\":true}", actionsDecoded["initializationOptions"])
	}

	yamlParams := yamlFake.Received()[0].Params
	var yamlDecoded map[string]json.RawMessage
	_ = json.Unmarshal(yamlParams, &yamlDecoded)
	if _, present := yamlDecoded["initializationOptions"]; present {
		t.Errorf("yaml: initializationOptions = %s, want field removed (unset in config)", yamlDecoded["initializationOptions"])
	}
}

func TestSession_InitializePartialFailure(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{"hoverProvider":true}`))
	yamlFake := newFakeServer(json.RawMessage(`{}`))
	yamlFake.initErr = &fakeInitError
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestSession(t, twoServerConfig(), byName)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error != nil {
		t.Fatalf("initialize: unexpected error (one server failing should not fail the session): %v", resp.Error)
	}

	var result struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var caps map[string]json.RawMessage
	_ = json.Unmarshal(result.Capabilities, &caps)
	if v, ok := asJSONBool(caps["hoverProvider"]); !ok || !v {
		t.Errorf("hoverProvider = %s, want true (from the surviving server)", caps["hoverProvider"])
	}
}

func TestSession_InitializeAllFail(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{}`))
	actionsFake.initErr = &fakeInitError
	yamlFake := newFakeServer(json.RawMessage(`{}`))
	yamlFake.initErr = &fakeInitError
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestSession(t, twoServerConfig(), byName)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error == nil {
		t.Fatal("initialize: expected error when every server fails, got success")
	}
}

func TestSession_InitializedShutdownExit(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{}`))
	yamlFake := newFakeServer(json.RawMessage(`{}`))
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestSession(t, twoServerConfig(), byName)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error != nil {
		t.Fatalf("initialize: %v", resp.Error)
	}

	if err := client.Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("notify initialized: %v", err)
	}

	shutdownResp := mustCall(t, client, "shutdown", nil)
	if shutdownResp.Error != nil {
		t.Fatalf("shutdown: unexpected error: %v", shutdownResp.Error)
	}

	if err := client.Notify("exit", nil); err != nil {
		t.Fatalf("notify exit: %v", err)
	}

	// Wait for exit's fan-out (send exit, grace period, terminate) to
	// finish on both fakes; termination tears down their connections.
	deadline := time.After(exitGracePeriod + 3*time.Second)
	for _, fake := range byName {
		select {
		case <-fake.conn.Done():
		case <-deadline:
			t.Fatal("timed out waiting for downstream connection to close after exit")
		}
	}

	for name, fake := range byName {
		var methods []string
		for _, r := range fake.Received() {
			methods = append(methods, r.Method)
		}
		want := []string{"initialize", "initialized", "shutdown", "exit"}
		if len(methods) != len(want) {
			t.Fatalf("%s: received %v, want %v", name, methods, want)
		}
		for i := range want {
			if methods[i] != want[i] {
				t.Errorf("%s: received[%d] = %q, want %q", name, i, methods[i], want[i])
			}
		}
	}
}
