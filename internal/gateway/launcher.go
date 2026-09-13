// Package gateway implements the stateful half of rolling-star: the
// upstream-facing Session that behaves as a single Language Server, the
// downstream connections it drives as an LSP Client, and the process
// management and lifecycle plumbing that ties them together.
package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/cross-ts/rolling-star/internal/config"
)

// Process is a running downstream language server, seen as a duplex byte
// stream plus the ability to wait for and force its termination. Read
// yields the child's stdout, Write feeds the child's stdin, and Close
// terminates the process (it does not merely close the pipes).
type Process interface {
	io.ReadWriteCloser
	Wait() error
}

// Launcher starts a downstream server described by def and returns a
// handle to it. It is the project's single test seam: production code
// uses ExecLauncher, while tests inject a Launcher that returns one end
// of a net.Pipe wired to a fake in-process server instead of spawning a
// real child process.
type Launcher func(ctx context.Context, def config.ServerDef) (Process, error)

// ExecLauncher launches def.Command as a real child process. The child's
// stdin/stdout are connected to the returned Process; its stderr is
// streamed line-by-line to our own stderr, each line prefixed with
// "[<server name>] ", so operators can tell downstream log lines apart
// without the child needing to know it is being proxied.
//
// The child's environment is os.Environ() plus def.Env (def.Env values
// already have os.ExpandEnv applied by config.Load).
func ExecLauncher(ctx context.Context, def config.ServerDef) (Process, error) {
	cmd := exec.CommandContext(ctx, def.Command, def.Args...)
	cmd.Env = os.Environ()
	for k, v := range def.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: stdin pipe: %w", def.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: stdout pipe: %w", def.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("gateway: %s: stderr pipe: %w", def.Name, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("gateway: %s: start: %w", def.Name, err)
	}

	go streamStderr(def.Name, stderr)

	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// streamStderr copies r line-by-line to os.Stderr, prefixing each line
// with "[name] ". It runs until r is closed (the child exited or its
// stderr pipe was otherwise torn down).
func streamStderr(name string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", name, scanner.Text())
	}
}

// execProcess adapts an *exec.Cmd plus its stdin/stdout pipes to Process.
type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (p *execProcess) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *execProcess) Write(b []byte) (int, error) { return p.stdin.Write(b) }

// Close terminates the process. It closes stdin first (many language
// servers treat EOF on stdin as a shutdown signal) then kills the
// process outright; callers that want a graceful exit sequence should
// send the LSP exit notification and give the process a grace period
// before calling Close.
func (p *execProcess) Close() error {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

func (p *execProcess) Wait() error { return p.cmd.Wait() }

// stdio adapts os.Stdin/os.Stdout to a single io.ReadWriteCloser for the
// upstream connection. Close is a no-op: the process owns its own
// standard streams and closing them here would be premature relative to
// process exit.
type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdio) Close() error                { return nil }

// Stdio returns the process's standard input/output as a single
// io.ReadWriteCloser, for use as the upstream transport in Session.Serve.
func Stdio() io.ReadWriteCloser { return stdio{} }
