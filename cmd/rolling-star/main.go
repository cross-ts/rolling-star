package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cross-ts/rolling-star/internal/config"
	"github.com/cross-ts/rolling-star/internal/gateway"
)

const signalTeardownTimeout = 10 * time.Second

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

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	sess, err := gateway.New(cfg, gateway.Options{Logger: log})
	if err != nil {
		log.Error("failed to build session", "error", err)
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveDone := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			log.Info("received signal; shutting down downstream servers", "signal", sig.String())

			teardownCtx, teardownCancel := context.WithTimeout(context.Background(), signalTeardownTimeout)
			defer teardownCancel()
			sess.Close(teardownCtx)
			cancel()

			os.Exit(exitCode(sess))
		case <-serveDone:
		}
	}()

	serveErr := sess.Serve(ctx, gateway.Stdio())
	close(serveDone)

	if serveErr != nil {
		log.Error("connection ended with an error", "error", serveErr)
	}

	return exitCode(sess)
}

func exitCode(sess *gateway.Session) int {
	if sess.ShutdownReceived() {
		return 0
	}
	return 1
}
