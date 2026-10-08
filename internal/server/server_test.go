package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newTestKey generates a throwaway unencrypted ED25519 keypair in the test's
// temporary directory and returns the private key path.
func newTestKey(t *testing.T) string {
	t.Helper()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available:", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	cmd := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "test@example.com", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate test key: %v: %s", err, out)
	}
	return path
}

// newSigner builds a real ssh-keygen signer over a throwaway key and returns it
// with the matching derived public key.
func newSigner(t *testing.T) (signing.Signer, string) {
	t.Helper()
	keyPath := newTestKey(t)
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	publicKey, err := signing.PublicKey(context.Background(), keyPath)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	return signer, publicKey
}

func newTestHTTPServer(t *testing.T, signer signing.Signer, publicKey string, cfg server.Config) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.New(signer, publicKey, cfg, discardLogger()))
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s: %v", url, err)
	}
	return resp, string(body)
}

func TestHealthzAlwaysAlive(t *testing.T) {
	// No key and no signer configured: liveness must still succeed.
	ts := newTestHTTPServer(t, nil, "", server.Config{})

	resp, body := get(t, ts.URL+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("GET /healthz returned an empty body")
	}
}

func TestReadyzNotReadyWithoutKey(t *testing.T) {
	const keyPath = "/var/lib/git-signer/signing_key"
	ts := newTestHTTPServer(t, nil, "", server.Config{KeyPath: keyPath})

	resp, body := get(t, ts.URL+"/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz without key status = %d, want 503", resp.StatusCode)
	}
	if strings.Contains(body, keyPath) || strings.Contains(body, "PRIVATE KEY") {
		t.Fatalf("GET /readyz leaked key material or config: %q", body)
	}
}

func TestReadyzReadyWithKey(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, body := get(t, ts.URL+"/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz with key status = %d, want 200: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatalf("GET /readyz leaked key material: %q", body)
	}
}
