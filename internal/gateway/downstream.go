package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

// Downstream is one running language server plus the LSP connection
// rolling-star drives it over. rolling-star is this connection's client:
// it sends initialize/initialized/shutdown/exit and forwards routed
// document notifications and requests to it.
type Downstream struct {
	Def config.ServerDef

	proc Process
	conn *jsonrpc.Conn

	// sess is the Session this Downstream belongs to, used by Handle (see
	// serverhandler.go) to relay server->client traffic upstream. It is
	// set once, right after StartDownstream returns (see lifecycle.go's
	// handleInitialize). A nil sess (the T4/T5 default, still used by
	// tests that construct a Downstream directly) replies MethodNotFound
	// to requests and drops notifications.
	sess *Session
}

// StartDownstream launches def via launch (ExecLauncher if nil) and
// establishes a jsonrpc.Conn over it, running the connection's read loop
// in a background goroutine. It does not send initialize; call
// Downstream.Initialize for that.
func StartDownstream(ctx context.Context, def config.ServerDef, launch Launcher) (*Downstream, error) {
	if launch == nil {
		launch = ExecLauncher
	}

	d := &Downstream{Def: def}

	proc, err := launch(ctx, def)
	if err != nil {
		return nil, fmt.Errorf("gateway: start %s: %w", def.Name, err)
	}
	d.proc = proc
	d.conn = jsonrpc.NewConn(proc, d)

	go d.conn.Run(ctx) //nolint:errcheck // surfaced via Done()/subsequent Call failures

	return d, nil
}

// Conn returns the underlying JSON-RPC connection to this server.
func (d *Downstream) Conn() *jsonrpc.Conn { return d.conn }

// Done returns a channel closed once this server's connection's read
// loop has exited.
func (d *Downstream) Done() <-chan struct{} { return d.conn.Done() }

// Wait blocks until the underlying process has exited.
func (d *Downstream) Wait() error { return d.proc.Wait() }

// call sends a request and waits for its response, bounded by ctx. It
// factors out the select this package's two blocking downstream calls
// (Initialize, Shutdown) share.
func (d *Downstream) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	ch, err := d.conn.Call(method, params)
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: %s: %w", d.Def.Name, method, err)
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("gateway: %s: %s: %w", d.Def.Name, method, msg.Error)
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("gateway: %s: %s: %w", d.Def.Name, method, ctx.Err())
	}
}

// Initialize sends an "initialize" request with the given params, waits
// for the response, and returns its "capabilities" object.
func (d *Downstream) Initialize(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	result, err := d.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return nil, fmt.Errorf("gateway: %s: initialize: decode result: %w", d.Def.Name, err)
	}
	return decoded.Capabilities, nil
}

// Shutdown sends a "shutdown" request and waits for its response (or for
// ctx to be done, whichever comes first).
func (d *Downstream) Shutdown(ctx context.Context) error {
	_, err := d.call(ctx, "shutdown", nil)
	return err
}

// Terminate force-terminates the downstream process. It is safe to call
// even if the process has already exited.
func (d *Downstream) Terminate() error {
	if err := d.proc.Close(); err != nil {
		return fmt.Errorf("gateway: %s: terminate: %w", d.Def.Name, err)
	}
	return nil
}
