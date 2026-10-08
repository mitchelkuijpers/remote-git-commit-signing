// Command git-signer-server is the remote commit-signing server: it holds the
// single ED25519 signing key and exposes the HTTP signing API (default port
// 8000).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
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

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := run(context.Background(), os.Getenv, logger); err != nil {
		logger.Error("git-signer-server: fatal", "error", err.Error())
		os.Exit(1)
	}
}

// run builds and starts the server. It returns only when serving fails or the
// process is stopped.
func run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	cfg, err := server.LoadConfig(getenv)
	if err != nil {
		return err
	}

	// Load the key up front so a missing or unreadable key fails startup rather
	// than the first signing request. The private key never leaves the process.
	publicKey, err := signing.PublicKey(ctx, cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("load signing key: %w", err)
	}

	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{
		KeyPath:         cfg.KeyPath,
		MaxPayloadBytes: cfg.MaxPayloadBytes,
		Timeout:         cfg.SignTimeout,
	})
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           server.New(signer, publicKey, cfg, logger),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	logger.Info("git-signer-server: listening", "addr", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
