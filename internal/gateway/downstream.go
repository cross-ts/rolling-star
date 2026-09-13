package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type Downstream struct {
	Def config.ServerDef

	proc Process
	conn *jsonrpc.Conn

	sess *Session
}

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

	go d.conn.Run(ctx)

	return d, nil
}

func (d *Downstream) Conn() *jsonrpc.Conn { return d.conn }

func (d *Downstream) Done() <-chan struct{} { return d.conn.Done() }

func (d *Downstream) Wait() error { return d.proc.Wait() }

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

func (d *Downstream) Shutdown(ctx context.Context) error {
	_, err := d.call(ctx, "shutdown", nil)
	return err
}

func (d *Downstream) Terminate() error {
	if err := d.proc.Close(); err != nil {
		return fmt.Errorf("gateway: %s: terminate: %w", d.Def.Name, err)
	}
	return nil
}
