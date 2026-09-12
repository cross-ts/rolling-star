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
// it sends initialize/initialized/shutdown/exit and (once T6 lands)
// forwards routed document notifications and requests to it.
type Downstream struct {
	Def config.ServerDef

	proc Process
	conn *jsonrpc.Conn
	caps json.RawMessage

	// handler processes requests/notifications the server sends us (the
	// server -> client direction: publishDiagnostics, logMessage,
	// registerCapability, workspace/configuration, custom methods, ...).
	// Routing/forwarding that traffic to the upstream client is T6's job;
	// it is injected here (see StartDownstream) rather than hard-coded so
	// this file does not need to change when T6 lands. A nil handler
	// (the T4/T5 default) replies MethodNotFound to requests and drops
	// notifications.
	handler jsonrpc.Handler
}

// StartDownstream launches def via launch (ExecLauncher if nil) and
// establishes a jsonrpc.Conn over it, running the connection's read loop
// in a background goroutine. It does not send initialize; call
// Downstream.Initialize for that.
func StartDownstream(ctx context.Context, def config.ServerDef, launch Launcher, handler jsonrpc.Handler) (*Downstream, error) {
	if launch == nil {
		launch = ExecLauncher
	}

	d := &Downstream{Def: def, handler: handler}

	proc, err := launch(ctx, def)
	if err != nil {
		return nil, fmt.Errorf("gateway: start %s: %w", def.Name, err)
	}
	d.proc = proc
	d.conn = jsonrpc.NewConn(proc, d)

	go d.conn.Run(ctx) //nolint:errcheck // surfaced via Done()/subsequent Call failures

	return d, nil
}

// Handle implements jsonrpc.Handler by delegating to the injected
// server->client handler, if any.
func (d *Downstream) Handle(ctx context.Context, c *jsonrpc.Conn, m *jsonrpc.Message) {
	if d.handler != nil {
		d.handler.Handle(ctx, c, m)
		return
	}
	if m.IsRequest() {
		_ = c.Reply(*m.ID, nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeMethodNotFound,
			Message: fmt.Sprintf("rolling-star: no downstream handler configured for %s", m.Method),
		})
	}
	// Notifications with nowhere to go are dropped.
}

// Conn returns the underlying JSON-RPC connection to this server.
func (d *Downstream) Conn() *jsonrpc.Conn { return d.conn }

// Capabilities returns the "capabilities" object captured from this
// server's initialize response, or nil if Initialize has not
// successfully completed yet.
func (d *Downstream) Capabilities() json.RawMessage { return d.caps }

// Done returns a channel closed once this server's connection's read
// loop has exited.
func (d *Downstream) Done() <-chan struct{} { return d.conn.Done() }

// Wait blocks until the underlying process has exited.
func (d *Downstream) Wait() error { return d.proc.Wait() }

// Initialize sends an "initialize" request with the given params, waits
// for the response, and captures its "capabilities" object (available
// afterwards via Capabilities). It returns the full raw result on
// success.
func (d *Downstream) Initialize(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	ch, err := d.conn.Call("initialize", params)
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: initialize: %w", d.Def.Name, err)
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("gateway: %s: initialize: %w", d.Def.Name, msg.Error)
		}
		var result struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			return nil, fmt.Errorf("gateway: %s: initialize: decode result: %w", d.Def.Name, err)
		}
		d.caps = result.Capabilities
		return msg.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("gateway: %s: initialize: %w", d.Def.Name, ctx.Err())
	}
}

// Initialized sends the "initialized" notification.
func (d *Downstream) Initialized() error {
	if err := d.conn.Notify("initialized", json.RawMessage(`{}`)); err != nil {
		return fmt.Errorf("gateway: %s: initialized: %w", d.Def.Name, err)
	}
	return nil
}

// Shutdown sends a "shutdown" request and waits for its response (or for
// ctx to be done, whichever comes first).
func (d *Downstream) Shutdown(ctx context.Context) error {
	ch, err := d.conn.Call("shutdown", nil)
	if err != nil {
		return fmt.Errorf("gateway: %s: shutdown: %w", d.Def.Name, err)
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("gateway: %s: shutdown: %w", d.Def.Name, msg.Error)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gateway: %s: shutdown: %w", d.Def.Name, ctx.Err())
	}
}

// Exit sends the "exit" notification.
func (d *Downstream) Exit() error {
	if err := d.conn.Notify("exit", nil); err != nil {
		return fmt.Errorf("gateway: %s: exit: %w", d.Def.Name, err)
	}
	return nil
}

// Terminate force-terminates the downstream process. It is safe to call
// even if the process has already exited.
func (d *Downstream) Terminate() error {
	if err := d.proc.Close(); err != nil {
		return fmt.Errorf("gateway: %s: terminate: %w", d.Def.Name, err)
	}
	return nil
}
