package languageserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

var fakeInitError = jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "fake: initialize failed"}

func TestStart_InitializeCapabilitiesShutdown(t *testing.T) {
	caps := json.RawMessage(`{"hoverProvider":true,"definitionProvider":true}`)
	fs := newFakeServer(caps)
	launch := newFakeLauncher(fs)

	def := config.LanguageServer{Name: "fake", Command: "irrelevant"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server, err := start(ctx, def, launch)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() { _ = server.Run(ctx, nil) }()

	gotCaps, err := server.Initialize(ctx, json.RawMessage(`{"processId":1}`))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if string(gotCaps) != string(caps) {
		t.Fatalf("Initialize capabilities = %s, want %s", gotCaps, caps)
	}

	if err := server.Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Notify initialized: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 2*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := server.Notify("exit", nil); err != nil {
		t.Fatalf("Notify exit: %v", err)
	}

	waitForReceipt(t, fs, "exit")

	if err := server.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	select {
	case <-server.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("language server connection did not close after Terminate")
	}

	assertMethods(t, fs, "initialize", "initialized", "shutdown", "exit")
}

func TestStart_InitializeFailure(t *testing.T) {
	fs := newFakeServer(json.RawMessage(`{}`))
	fs.initErr = &fakeInitError
	launch := newFakeLauncher(fs)

	def := config.LanguageServer{Name: "fake", Command: "irrelevant"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server, err := start(ctx, def, launch)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	go func() { _ = server.Run(ctx, nil) }()

	if _, err := server.Initialize(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Initialize: expected error, got nil")
	}
}
