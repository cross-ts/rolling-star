package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/gateway"
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

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return 1
	}

	sess, err := gateway.New(cfg, gateway.Options{Logger: logger})
	if err != nil {
		logger.Error("failed to build session", "error", err)
		return 1
	}

	serveErr := sess.Serve(context.Background(), gateway.Stdio())

	if serveErr != nil {
		logger.Error("connection ended with an error", "error", serveErr)
	}

	return exitCode(sess)
}

func exitCode(sess *gateway.Session) int {
	if sess.ShutdownReceived() {
		return 0
	}
	return 1
}
