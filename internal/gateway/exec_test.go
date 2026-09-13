package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

func TestMain(m *testing.M) {
	if os.Getenv("ROLLING_STAR_FAKE_LS") == "1" {
		runFakeLanguageServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type execFakeHandler struct{}

func (execFakeHandler) Handle(_ context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	switch m.Method {
	case "initialize":
		result, _ := json.Marshal(map[string]json.RawMessage{
			"capabilities": json.RawMessage(`{"hoverProvider":true}`),
		})
		_ = c.Reply(*m.ID, result, nil)
	case "shutdown":
		_ = c.Reply(*m.ID, nil, nil)
	case "exit":
		os.Exit(0)
	}
}

func runFakeLanguageServer() {
	conn := jsonrpc.NewConn(Stdio())
	_ = runMessages(context.Background(), conn, execFakeHandler{})
}

func TestExecLauncher_RealProcess(t *testing.T) {
	t.Setenv("ROLLING_STAR_FAKE_LS", "1")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	def := config.LanguageServer{
		Name:    "fakels",
		Command: exe,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server, err := StartLanguageServer(ctx, def, nil)
	if err != nil {
		t.Fatalf("StartLanguageServer: %v", err)
	}
	go func() { _ = runMessages(ctx, server.Conn(), nil) }()

	caps, err := server.Initialize(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if string(caps) != `{"hoverProvider":true}` {
		t.Errorf("Initialize capabilities = %s, want {\"hoverProvider\":true}", caps)
	}

	if err := server.Conn().Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Notify initialized: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := server.Conn().Notify("exit", nil); err != nil {
		t.Fatalf("Notify exit: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- server.Wait() }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("real language server process did not exit within the grace period after \"exit\"")
	}

	if err := server.Terminate(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Terminate after clean exit: unexpected error: %v", err)
	}
}
