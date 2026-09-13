package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/gateway"
	"github.com/cross-ts/rolling-star/internal/lsp"
	"github.com/cross-ts/rolling-star/internal/transport"
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

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}

	// TODO: 全体のConfigとgatewayのConfigを分離する
	g, err := gateway.New(cfg.Servers)
	if err != nil {
		slog.Error("failed to build gateway", "error", err)
		return 1
	}

	if err := g.Serve(context.Background(), lsp.NewClient(transport.Stdio())); err != nil {
		slog.Error("connection ended with an error", "error", err)
		return 1
	}

	return 0
}
