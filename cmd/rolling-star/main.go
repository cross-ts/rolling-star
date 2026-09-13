package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/cross-ts/rolling-star/internal/client"
	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/gateway"
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

	logger := slog.Default()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return 1
	}

	// TODO: 全体のConfigとgatewayのConfigを分離する
	g, err := gateway.New(cfg.Servers)
	if err != nil {
		logger.Error("failed to build gateway", "error", err)
		return 1
	}

	serveErr := g.Serve(context.Background(), client.New(transport.Stdio()))

	if serveErr != nil {
		logger.Error("connection ended with an error", "error", serveErr)
	}

	return exitCode(g)
}

func exitCode(g *gateway.Gateway) int {
	if g.ShutdownReceived() {
		return 0
	}
	return 1
}
