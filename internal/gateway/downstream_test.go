package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

var fakeInitError = jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "fake: initialize failed"}

func TestStartDownstream_InitializeCapabilitiesShutdown(t *testing.T) {
	caps := json.RawMessage(`{"hoverProvider":true,"definitionProvider":true}`)
	fs := newFakeServer(caps)
	launch := newFakeLauncher(fs)

	def := config.ServerDef{Name: "fake", Command: "irrelevant"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := StartDownstream(ctx, def, launch)
	if err != nil {
		t.Fatalf("StartDownstream: %v", err)
	}

	gotCaps, err := d.Initialize(ctx, json.RawMessage(`{"processId":1}`))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if string(gotCaps) != string(caps) {
		t.Fatalf("Initialize capabilities = %s, want %s", gotCaps, caps)
	}

	if err := d.Conn().Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Notify initialized: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 2*time.Second)
	defer shutdownCancel()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := d.Conn().Notify("exit", nil); err != nil {
		t.Fatalf("Notify exit: %v", err)
	}

	waitForReceipt(t, fs, "exit")

	if err := d.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	select {
	case <-d.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("downstream connection did not close after Terminate")
	}

	assertMethods(t, fs, "initialize", "initialized", "shutdown", "exit")
}

func TestStartDownstream_InitializeFailure(t *testing.T) {
	fs := newFakeServer(json.RawMessage(`{}`))
	fs.initErr = &fakeInitError
	launch := newFakeLauncher(fs)

	def := config.ServerDef{Name: "fake", Command: "irrelevant"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := StartDownstream(ctx, def, launch)
	if err != nil {
		t.Fatalf("StartDownstream: %v", err)
	}

	if _, err := d.Initialize(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Initialize: expected error, got nil")
	}
}
