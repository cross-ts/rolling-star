package gateway

import (
	"io"
	"os"
)

type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdio) Close() error                { return nil }

func Stdio() io.ReadWriteCloser { return stdio{} }
