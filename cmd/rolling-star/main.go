// Command rolling-star is the LSP Router Gateway entrypoint.
//
// INVARIANT: nothing but the upstream LSP framing writer (see
// internal/jsonrpc, wired here via gateway.Stdio) may write to
// os.Stdout. Stdout carries only Content-Length framed JSON-RPC messages
// to the upstream client; all logging MUST go to os.Stderr. This is why
// the *only* thing this file constructs on os.Stdout's behalf is the
// single gateway.Stdio() value handed to Session.Serve -- every other
// line of output in this file, and everything logged from
// internal/gateway, goes through slog to os.Stderr.
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

// signalTeardownTimeout bounds how long an interrupted rolling-star
// spends trying to shut its downstream servers down gracefully before
// giving up and exiting anyway. It intentionally does not block forever:
// the whole point of handling the signal is to exit promptly.
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

	// config.Load already produces a message naming the file and the
	// specific problem (open error, decode error, or a Validate
	// complaint); re-wrapping it here would only bury that detail.
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

	// A signal is not part of the LSP protocol: no "shutdown"/"exit" was
	// ever received from the client, so there is no message to reply to
	// and Session.Serve's blocking read of os.Stdin cannot be interrupted
	// by cancelling ctx alone. What we CAN and MUST still do is tear the
	// downstream child processes down gracefully instead of leaving them
	// orphaned, then exit -- so on a signal we call Session.Close directly
	// and exit from this goroutine rather than waiting for Serve to
	// return.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	serveDone := make(chan struct{})
	go func() {
		select {
		case sig := <-sigCh:
			log.Info("received signal; shutting down downstream servers", "signal", sig.String())

			// Tear down BEFORE cancelling ctx: ctx is the same context
			// gateway.New's downstream servers were launched with (via
			// exec.CommandContext), and CommandContext kills its process
			// the instant its context is Done. Cancelling first would
			// race our own graceful "shutdown"/"exit" sequence with an
			// immediate hard kill of every downstream. Cancelling after
			// Close has returned is just cleanup of ctx's own resources;
			// nothing downstream is still relying on it by then.
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

// exitCode implements the LSP-mandated exit status: 0 if "shutdown" was
// received before the connection ended (whether via "exit" or the
// client simply closing the connection), 1 otherwise -- signaling that
// this was not a clean, negotiated shutdown.
func exitCode(sess *gateway.Session) int {
	if sess.ShutdownReceived() {
		return 0
	}
	return 1
}
