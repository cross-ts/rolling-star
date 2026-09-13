package languageserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/jsonrpc"
)

type Server struct {
	definition config.LanguageServer

	proc Process
	conn *jsonrpc.Conn
}

func New(def config.LanguageServer, proc Process) *Server {
	return &Server{
		definition: def,
		proc:       proc,
		conn:       jsonrpc.NewConn(proc),
	}
}

func Start(ctx context.Context, def config.LanguageServer) (*Server, error) {
	return start(ctx, def, execLauncher)
}

func start(ctx context.Context, def config.LanguageServer, launch launcher) (*Server, error) {
	if launch == nil {
		launch = execLauncher
	}

	proc, err := launch(ctx, def)
	if err != nil {
		return nil, fmt.Errorf("languageserver: start %s: %w", def.Name, err)
	}
	return New(def, proc), nil
}

func (s *Server) Name() string { return s.definition.Name }

func (s *Server) Conn() *jsonrpc.Conn { return s.conn }

func (s *Server) Done() <-chan struct{} { return s.conn.Done() }

func (s *Server) Run(ctx context.Context, handle func(*jsonrpc.Message)) error {
	done := make(chan error, 1)
	go func() { done <- s.conn.Run(ctx) }()

	for {
		select {
		case m, ok := <-s.conn.Messages():
			if !ok {
				return <-done
			}
			if handle != nil {
				handle(m)
			}
		case err := <-done:
			return err
		case <-ctx.Done():
			_ = s.conn.Close()
			return ctx.Err()
		}
	}
}

func (s *Server) Wait() error { return s.proc.Wait() }

func (s *Server) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	ch, err := s.conn.Call(method, params)
	if err != nil {
		return nil, fmt.Errorf("languageserver: %s: %s: %w", s.definition.Name, method, err)
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, fmt.Errorf("languageserver: %s: %s: %w", s.definition.Name, method, msg.Error)
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("languageserver: %s: %s: %w", s.definition.Name, method, ctx.Err())
	}
}

func (s *Server) Initialize(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	result, err := s.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &decoded); err != nil {
		return nil, fmt.Errorf("languageserver: %s: initialize: decode result: %w", s.definition.Name, err)
	}
	return decoded.Capabilities, nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	_, err := s.call(ctx, "shutdown", nil)
	return err
}

func (s *Server) Terminate() error {
	if err := s.proc.Close(); err != nil {
		return fmt.Errorf("languageserver: %s: terminate: %w", s.definition.Name, err)
	}
	return nil
}
