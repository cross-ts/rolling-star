package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type LanguageServer struct {
	definition config.LanguageServer

	proc Process
	conn *jsonrpc.Conn
}

func StartLanguageServer(ctx context.Context, def config.LanguageServer, launch Launcher) (*LanguageServer, error) {
	if launch == nil {
		launch = ExecLauncher
	}

	server := &LanguageServer{definition: def}

	proc, err := launch(ctx, def)
	if err != nil {
		return nil, fmt.Errorf("gateway: start %s: %w", def.Name, err)
	}
	server.proc = proc
	server.conn = jsonrpc.NewConn(proc)

	return server, nil
}

func (s *LanguageServer) Conn() *jsonrpc.Conn { return s.conn }

func (s *LanguageServer) Done() <-chan struct{} { return s.conn.Done() }

func (s *LanguageServer) Wait() error { return s.proc.Wait() }

func (s *LanguageServer) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	ch, err := s.conn.Call(method, params)
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: %s: %w", s.definition.Name, method, err)
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("gateway: %s: %s: %w", s.definition.Name, method, msg.Error)
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("gateway: %s: %s: %w", s.definition.Name, method, ctx.Err())
	}
}

func (s *LanguageServer) Initialize(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	result, err := s.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return nil, fmt.Errorf("gateway: %s: initialize: decode result: %w", s.definition.Name, err)
	}
	return decoded.Capabilities, nil
}

func (s *LanguageServer) Shutdown(ctx context.Context) error {
	_, err := s.call(ctx, "shutdown", nil)
	return err
}

func (s *LanguageServer) Terminate() error {
	if err := s.proc.Close(); err != nil {
		return fmt.Errorf("gateway: %s: terminate: %w", s.definition.Name, err)
	}
	return nil
}
