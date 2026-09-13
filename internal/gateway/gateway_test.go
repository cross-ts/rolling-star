package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	clientpkg "github.com/cross-ts/rolling-star/internal/client"
	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type noopHandler struct{}

func (noopHandler) Handle(context.Context, *jsonrpc.Conn, *jsonrpc.Message) {}

type eofTransport struct{}

func (eofTransport) Read([]byte) (int, error)    { return 0, io.EOF }
func (eofTransport) Write(p []byte) (int, error) { return len(p), nil }
func (eofTransport) Close() error                { return nil }

func testLogger(t *testing.T) *slog.Logger {
	if testing.Verbose() {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestGateway(t *testing.T, cfg *config.Config, byName map[string]*fakeServer) *jsonrpc.Conn {
	t.Helper()
	return newTestGatewayWithHandler(t, cfg, byName, noopHandler{})
}

func newTestGatewayWithHandler(t *testing.T, cfg *config.Config, byName map[string]*fakeServer, clientHandler messageHandler) *jsonrpc.Conn {
	t.Helper()

	g, err := New(cfg.Servers)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.startLanguageServer = newFakeLanguageServerFactory(byName)
	g.log = testLogger(t)

	clientSide, gatewaySide := net.Pipe()
	client := jsonrpc.NewConn(clientSide)
	gatewayClient := clientpkg.New(gatewaySide)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = runMessages(ctx, client, clientHandler) }()
	go g.Serve(ctx, gatewayClient)

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
		Servers: []config.LanguageServer{
			{
				Name:      "actions",
				Command:   "irrelevant",
				Selectors: []config.Selector{{Language: "yaml", Pattern: ".github/workflows/**/*.yml"}},
			},
			{
				Name:      "yaml",
				Command:   "irrelevant",
				Selectors: []config.Selector{{Language: "yaml"}},
			},
		},
	}
}

func TestGateway_ServeReturnsErrorBeforeShutdown(t *testing.T) {
	g, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = g.Serve(context.Background(), clientpkg.New(eofTransport{}))
	if !errors.Is(err, errClientDisconnectedBeforeShutdown) {
		t.Fatalf("Serve error = %v, want %v", err, errClientDisconnectedBeforeShutdown)
	}
}

func TestGateway_InitializeFanOut(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{"hoverProvider":true}`))
	yamlFake := newFakeServer(json.RawMessage(`{"definitionProvider":true}`))
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestGateway(t, twoServerConfig(), byName)

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
}

func TestGateway_InitializePartialFailure(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{"hoverProvider":true}`))
	yamlFake := newFakeServer(json.RawMessage(`{}`))
	yamlFake.initErr = &fakeInitError
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestGateway(t, twoServerConfig(), byName)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error != nil {
		t.Fatalf("initialize: unexpected error (one server failing should not fail the gateway): %v", resp.Error)
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

func TestGateway_InitializeAllFail(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{}`))
	actionsFake.initErr = &fakeInitError
	yamlFake := newFakeServer(json.RawMessage(`{}`))
	yamlFake.initErr = &fakeInitError
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestGateway(t, twoServerConfig(), byName)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error == nil {
		t.Fatal("initialize: expected error when every server fails, got success")
	}
}

func TestGateway_InitializeTimeoutDropsHungServer(t *testing.T) {
	original := languageServerInitializeTimeout
	languageServerInitializeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { languageServerInitializeTimeout = original })

	hungFake := newFakeServer(json.RawMessage(`{}`))
	hungFake.hangOnInitialize = true
	okFake := newFakeServer(json.RawMessage(`{"hoverProvider":true}`))
	byName := map[string]*fakeServer{"actions": hungFake, "yaml": okFake}

	client := newTestGateway(t, twoServerConfig(), byName)

	resp := mustCall(t, client, "initialize", json.RawMessage(`{"rootUri":"file:///repo"}`))
	if resp.Error != nil {
		t.Fatalf("initialize: unexpected error (the surviving server should still bring the gateway up): %v", resp.Error)
	}

	var result struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var caps map[string]json.RawMessage
	if err := json.Unmarshal(result.Capabilities, &caps); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if v, ok := asJSONBool(caps["hoverProvider"]); !ok || !v {
		t.Errorf("hoverProvider = %s, want true (from the surviving server; the hung one must have been dropped)", caps["hoverProvider"])
	}
}

func TestGateway_InitializedShutdownExit(t *testing.T) {
	actionsFake := newFakeServer(json.RawMessage(`{}`))
	yamlFake := newFakeServer(json.RawMessage(`{}`))
	byName := map[string]*fakeServer{"actions": actionsFake, "yaml": yamlFake}

	client := newTestGateway(t, twoServerConfig(), byName)

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

	for _, fake := range byName {
		waitFor(t, func() bool {
			select {
			case <-fake.conn.Done():
				return true
			default:
				return false
			}
		}, "timed out waiting for language server connection to close after exit")
	}

	for _, fake := range byName {
		assertMethods(t, fake, "initialize", "initialized", "shutdown", "exit")
	}
}
