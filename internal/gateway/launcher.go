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

type Process interface {
	io.ReadWriteCloser
	Wait() error
}

type Launcher func(ctx context.Context, def config.Server) (Process, error)

func ExecLauncher(ctx context.Context, def config.Server) (Process, error) {
	cmd := exec.CommandContext(ctx, def.Command, def.Args...)
	cmd.Env = os.Environ()

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

func streamStderr(name string, r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", name, scanner.Text())
	}
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
}

func (p *execProcess) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *execProcess) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *execProcess) Close() error {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		return p.cmd.Process.Kill()
	}
	return nil
}

func (p *execProcess) Wait() error { return p.cmd.Wait() }

type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdio) Close() error                { return nil }

func Stdio() io.ReadWriteCloser { return stdio{} }
