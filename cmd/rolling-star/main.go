// Command rolling-star is the LSP Router Gateway entrypoint.
//
// INVARIANT: nothing but the upstream LSP framing writer (see
// internal/jsonrpc) may write to os.Stdout. Stdout carries only
// Content-Length framed JSON-RPC messages to the upstream client;
// all logging MUST go to os.Stderr.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	os.Exit(run())
}

func run() int {
	fs := flag.NewFlagSet("rolling-star", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("c", "", "path to config file (required)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: rolling-star -c <config-path>\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *configPath == "" {
		fs.Usage()
		return 2
	}

	// TODO(T7): load config, wire internal/gateway.Session, and Serve
	// over internal/jsonrpc.Stdio(). For now this is a placeholder.
	fmt.Fprintln(os.Stderr, "rolling-star: not implemented yet")
	return 0
}
