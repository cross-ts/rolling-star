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

// TestMain lets this test binary double as the "downstream language
// server" for TestExecLauncher_RealProcess: when re-executed with
// ROLLING_STAR_FAKE_LS=1, it runs a tiny real language server over its
// own stdio instead of the test suite, so Tier 3 can exercise
// ExecLauncher against a real OS process/pipes without shipping a
// second binary.
func TestMain(m *testing.M) {
	if os.Getenv("ROLLING_STAR_FAKE_LS") == "1" {
		runFakeLanguageServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// execFakeHandler answers just enough of the lifecycle to be a usable
// downstream: initialize, shutdown, and a voluntary exit on "exit" (so
// the real-process teardown path can observe a well-behaved server that
// exits on its own, not just one that has to be killed).
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
	conn := jsonrpc.NewConn(Stdio(), execFakeHandler{})
	_ = conn.Run(context.Background())
}

// TestExecLauncher_RealProcess is the Tier 3 exec-level smoke test: a
// real child process, real OS pipes (not net.Pipe), and real process
// teardown, driven entirely through the same Downstream API the rest of
// the package uses. Unlike the net.Pipe fakeServer harness, every step
// here already blocks on the real round trip (Initialize/Shutdown wait
// on an actual response over an actual pipe), so -- learned the hard way
// in T6/T7 -- there is no "write returned, but the peer hasn't handled it
// yet" race to poll around for those calls; the one place that still
// needs a bounded wait rather than an immediate assertion is process
// exit itself, which happens in the child's own time.
func TestExecLauncher_RealProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	def := config.ServerDef{
		Name:    "fakels",
		Command: exe,
		Env:     map[string]string{"ROLLING_STAR_FAKE_LS": "1"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	d, err := StartDownstream(ctx, def, nil) // nil launcher -> ExecLauncher
	if err != nil {
		t.Fatalf("StartDownstream: %v", err)
	}

	caps, err := d.Initialize(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if string(caps) != `{"hoverProvider":true}` {
		t.Errorf("Initialize capabilities = %s, want {\"hoverProvider\":true}", caps)
	}

	if err := d.Conn().Notify("initialized", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Notify initialized: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
	defer shutdownCancel()
	if err := d.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := d.Conn().Notify("exit", nil); err != nil {
		t.Fatalf("Notify exit: %v", err)
	}

	// The child exits voluntarily on "exit"; wait for the real process
	// to go away rather than asserting immediately.
	waited := make(chan error, 1)
	go func() { waited <- d.Wait() }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("real downstream process did not exit within the grace period after \"exit\"")
	}

	// Terminate after a clean voluntary exit should report
	// os.ErrProcessDone (already gone), not a real failure.
	if err := d.Terminate(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Terminate after clean exit: unexpected error: %v", err)
	}
}
