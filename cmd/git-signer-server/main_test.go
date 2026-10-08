package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestServeWithShutdownFinishesInFlightRequest verifies the graceful-shutdown
// wiring: once the context is cancelled (the same path SIGTERM drives in
// main), an already in-flight request is allowed to complete and the server
// returns cleanly.
func TestServeWithShutdownFinishesInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("signature"))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	srv := &http.Server{Handler: handler}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- serveWithShutdown(ctx, srv, ln, 5*time.Second, discardLogger())
	}()

	type result struct {
		body string
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/v1/sign")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		resCh <- result{body: string(body), err: err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the request")
	}

	// Equivalent of SIGTERM: the request is in flight when shutdown begins.
	cancel()
	close(release)

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("in-flight request dropped: %v", res.err)
		}
		if res.body != "signature" {
			t.Fatalf("body = %q, want %q", res.body, "signature")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serveWithShutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveWithShutdown did not return")
	}

	// The listener must be closed once shutdown has returned.
	if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}

// TestServeWithShutdownPropagatesServeError verifies a listener/serve failure
// is surfaced instead of being swallowed (the server dies visibly).
func TestServeWithShutdownPropagatesServeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	srv := &http.Server{Handler: http.NewServeMux()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := serveWithShutdown(ctx, srv, ln, time.Second, discardLogger()); err == nil {
		t.Fatal("expected an error from the closed listener, got nil")
	}
}

func envFrom(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// TestRunFailsWithoutKeyPath covers fail-fast startup: a missing key path is a
// configuration error, not a deferred one.
func TestRunFailsWithoutKeyPath(t *testing.T) {
	err := run(context.Background(), envFrom(nil), discardLogger())
	if err == nil {
		t.Fatal("expected an error when SIGNER_KEY_PATH is unset")
	}
	if !strings.Contains(err.Error(), "SIGNER_KEY_PATH") {
		t.Fatalf("error = %q, want it to mention SIGNER_KEY_PATH", err)
	}
}

// TestRunFailsWhenKeyMissing covers fail-fast startup when the key file does not
// exist: startup returns an error instead of serving and failing later.
func TestRunFailsWhenKeyMissing(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "signing_key")
	err := run(context.Background(), envFrom(map[string]string{
		"SIGNER_KEY_PATH":        keyPath,
		"SIGNER_COMMITTER_NAME":  "Test Signer",
		"SIGNER_COMMITTER_EMAIL": "signer@example.com",
	}), discardLogger())
	if err == nil {
		t.Fatal("expected an error when the signing key is missing")
	}
	if !strings.Contains(err.Error(), "load signing key") {
		t.Fatalf("error = %q, want it to mention loading the signing key", err)
	}
}

// TestRunFailsWhenKeyPermissionsTooOpen covers fail-fast startup on a key that
// is readable by other users: ssh-keygen refuses it, and startup must fail
// visibly rather than run with a weakly protected key.
func TestRunFailsWhenKeyPermissionsTooOpen(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "signing_key")
	// Content is irrelevant: ssh-keygen rejects the file on permissions first.
	if err := os.WriteFile(keyPath, []byte("not a real key\n"), 0o644); err != nil {
		t.Fatalf("write key: %v", err)
	}
	// Ensure the mode is not masked by the umask.
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod key: %v", err)
	}

	err := run(context.Background(), envFrom(map[string]string{
		"SIGNER_KEY_PATH":        keyPath,
		"SIGNER_COMMITTER_NAME":  "Test Signer",
		"SIGNER_COMMITTER_EMAIL": "signer@example.com",
	}), discardLogger())
	if err == nil {
		t.Fatal("expected an error for a world-readable signing key")
	}
	if !strings.Contains(err.Error(), "load signing key") {
		t.Fatalf("error = %q, want it to mention loading the signing key", err)
	}
}
