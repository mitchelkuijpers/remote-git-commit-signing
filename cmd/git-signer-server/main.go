// Command git-signer-server is the remote commit-signing server: it holds the
// single ED25519 signing key and exposes the HTTP signing API (default port
// 8000).
//
// The process shuts down gracefully: on SIGTERM or SIGINT it stops accepting
// new connections and gives in-flight signatures up to shutdownTimeout to
// finish before exiting. This is what lets systemd stop the service without
// dropping a signature that a client is waiting for.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// HTTP server timeouts. Each signing request additionally enforces its own
// (shorter) deadline inside the handler.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 60 * time.Second
)

// shutdownTimeout bounds how long shutdown waits for in-flight requests. It is
// deliberately shorter than the systemd unit's TimeoutStopSec so the process
// exits before systemd escalates to SIGKILL.
const shutdownTimeout = 25 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	// SIGTERM/SIGINT cancel this context, which drives graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Getenv, logger); err != nil {
		logger.Error("git-signer-server: fatal", "error", err.Error())
		os.Exit(1)
	}
}

// run builds and starts the server. It returns when serving fails or the
// context is cancelled (SIGTERM/SIGINT) and shutdown completes.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	cfg, err := server.LoadConfig(getenv)
	if err != nil {
		return err
	}

	// Load the key up front so a missing, unreadable, or loosely-permissioned
	// key fails startup (visibly, in journald) rather than the first signing
	// request. The private key never leaves the process.
	publicKey, err := signing.PublicKey(ctx, cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("load signing key: %w", err)
	}

	handlerCfg := cfg.WithDefaults()
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{
		KeyPath:         cfg.KeyPath,
		MaxPayloadBytes: handlerCfg.MaxPayloadBytes,
		Timeout:         handlerCfg.SignTimeout,
	})
	if err != nil {
		return err
	}

	// Bind explicitly so a port conflict is reported at startup and the test
	// seam can serve a listener on an ephemeral port.
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.Port))
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	httpServer := &http.Server{
		Handler:           server.New(signer, publicKey, handlerCfg, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	logger.Info("git-signer-server: listening", "addr", listener.Addr().String())
	return serveWithShutdown(ctx, httpServer, listener, shutdownTimeout, logger)
}

// serveWithShutdown serves on listener until either serving fails or ctx is
// cancelled, then shuts down gracefully: new connections are refused while
// requests already being handled are allowed up to shutdownTimeout to finish.
func serveWithShutdown(ctx context.Context, srv *http.Server, listener net.Listener, shutdownTimeout time.Duration, logger *slog.Logger) error {
	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		// Serving stopped on its own (bind/serve failure): nothing to drain.
		return err
	case <-ctx.Done():
	}

	logger.Info("git-signer-server: shutting down", "timeout", shutdownTimeout.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return <-serveErr
}
