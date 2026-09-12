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

	d, err := StartDownstream(ctx, def, launch, nil)
	if err != nil {
		t.Fatalf("StartDownstream: %v", err)
	}

	result, err := d.Initialize(ctx, json.RawMessage(`{"processId":1}`))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result == nil {
		t.Fatalf("Initialize returned nil result")
	}

	if string(d.Capabilities()) != string(caps) {
		t.Fatalf("Capabilities() = %s, want %s", d.Capabilities(), caps)
	}

	if err := d.Initialized(); err != nil {
		t.Fatalf("Initialized: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 2*time.Second)
	defer shutdownCancel()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := d.Exit(); err != nil {
		t.Fatalf("Exit: %v", err)
	}
	// Notify's underlying write returning only means the fake has read the
	// bytes off the pipe, not that its Handle (running in the fake's own
	// goroutine) has appended to its receipts yet; wait for that before
	// asserting on Received() or terminating out from under it.
	waitForReceipt(t, fs, "exit")

	if err := d.Terminate(); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	select {
	case <-d.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("downstream connection did not close after Terminate")
	}

	methods := make([]string, 0)
	for _, r := range fs.Received() {
		methods = append(methods, r.Method)
	}
	want := []string{"initialize", "initialized", "shutdown", "exit"}
	if len(methods) != len(want) {
		t.Fatalf("fake received %v, want %v", methods, want)
	}
	for i, m := range want {
		if methods[i] != m {
			t.Errorf("received[%d] = %q, want %q", i, methods[i], m)
		}
	}
}

func TestStartDownstream_InitializeFailure(t *testing.T) {
	fs := newFakeServer(json.RawMessage(`{}`))
	fs.initErr = &fakeInitError
	launch := newFakeLauncher(fs)

	def := config.ServerDef{Name: "fake", Command: "irrelevant"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := StartDownstream(ctx, def, launch, nil)
	if err != nil {
		t.Fatalf("StartDownstream: %v", err)
	}

	if _, err := d.Initialize(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("Initialize: expected error, got nil")
	}
}
