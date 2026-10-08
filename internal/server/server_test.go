package server_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestPublicKeyServesDerivedKey(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, body := get(t, ts.URL+"/v1/public-key")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/public-key status = %d, want 200: %s", resp.StatusCode, body)
	}
	fields := strings.Fields(publicKey)
	if len(fields) < 2 || !strings.Contains(body, fields[0]+" "+fields[1]) {
		t.Fatalf("GET /v1/public-key body %q does not contain the derived public key %q", body, publicKey)
	}
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatalf("GET /v1/public-key leaked private material: %q", body)
	}
}

func TestPublicKeyNotServedWithoutKey(t *testing.T) {
	ts := newTestHTTPServer(t, nil, "", server.Config{})

	resp, body := get(t, ts.URL+"/v1/public-key")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /v1/public-key without key status = %d, want 503: %s", resp.StatusCode, body)
	}
}

func TestSignRoundTrip(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Test <test@example.com> 1700000000 +0000\ncommitter Test <test@example.com> 1700000000 +0000\n\nsubject line\n")

	resp, sig := postSign(t, ts.URL, payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/sign status = %d, want 200: %s", resp.StatusCode, sig)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/vnd.sshsig" {
		t.Fatalf("POST /v1/sign Content-Type = %q, want application/vnd.sshsig", got)
	}
	if !strings.HasPrefix(sig, "-----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("POST /v1/sign body is not an SSHSIG PEM block: %q", sig)
	}
	if strings.Contains(sig, "PRIVATE KEY") {
		t.Fatalf("POST /v1/sign leaked private material: %q", sig)
	}

	// Independent verification: stock ssh-keygen must accept the signature
	// over the exact payload against the served public key.
	verifySignature(t, publicKey, payload, sig)
}

func TestSignRefusesWithoutKey(t *testing.T) {
	ts := newTestHTTPServer(t, nil, "", server.Config{})

	resp, body := postSign(t, ts.URL, []byte("payload"))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/sign without key status = %d, want 503: %s", resp.StatusCode, body)
	}
}

// postSign sends payload to the sign endpoint as an octet-stream.
func postSign(t *testing.T, baseURL string, payload []byte) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/sign", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new sign request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/sign: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read sign response: %v", err)
	}
	return resp, string(body)
}

func TestSignRejectsOversizedBody(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{MaxPayloadBytes: 64})

	resp, body := postSign(t, ts.URL, bytes.Repeat([]byte("A"), 65))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST oversized status = %d, want 413: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "AAAA") {
		t.Fatalf("POST oversized body leaked payload bytes: %q", body)
	}

	// A payload exactly at the limit is still signed.
	resp, sig := postSign(t, ts.URL, bytes.Repeat([]byte("A"), 64))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST at limit status = %d, want 200: %s", resp.StatusCode, sig)
	}
}

func TestSignRejectsUnreadableBody(t *testing.T) {
	signer, publicKey := newSigner(t)
	h := server.New(signer, publicKey, server.Config{}, discardLogger())

	req := httptest.NewRequest(http.MethodPost, "/v1/sign", failingReader{err: errors.New("client read failure sentinel")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST unreadable body status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "client read failure sentinel") {
		t.Fatalf("POST unreadable body echoed the read error: %q", rec.Body.String())
	}
}

func TestSignInternalFailureReturns500(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "missing-key")
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	ts := newTestHTTPServer(t, signer, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDummyDummyDummy", server.Config{})

	resp, body := postSign(t, ts.URL, []byte("payload"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("POST with broken signer status = %d, want 500: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, keyPath) {
		t.Fatalf("POST signing failure leaked the key path: %q", body)
	}
}

func TestSignRejectsWrongMethod(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, _ := get(t, ts.URL+"/v1/sign")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/sign status = %d, want 405", resp.StatusCode)
	}
}

func TestResponsesNeverLeakPrivateKey(t *testing.T) {
	keyPath := newTestKey(t)
	privateKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	publicKey, err := signing.PublicKey(context.Background(), keyPath)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{MaxPayloadBytes: 32})

	assertNoKeyMaterial := func(t *testing.T, body string) {
		t.Helper()
		if strings.Contains(body, "PRIVATE KEY") || strings.Contains(body, strings.TrimSpace(string(privateKey))) {
			t.Fatalf("response leaked private key material: %q", body)
		}
	}

	for _, path := range []string{"/healthz", "/readyz", "/v1/public-key"} {
		_, body := get(t, ts.URL+path)
		assertNoKeyMaterial(t, body)
	}

	_, sig := postSign(t, ts.URL, []byte("tree deadbeef\n\nsubject\n"))
	assertNoKeyMaterial(t, sig)

	_, oversizeBody := postSign(t, ts.URL, bytes.Repeat([]byte("A"), 33))
	assertNoKeyMaterial(t, oversizeBody)

	rec := httptest.NewRecorder()
	server.New(signer, publicKey, server.Config{}, discardLogger()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/sign", failingReader{err: errors.New("boom")}))
	assertNoKeyMaterial(t, rec.Body.String())
}

// failingReader fails immediately, simulating a request body that cannot be
// read.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// verifySignature asserts that stock ssh-keygen verifies sig over payload in
// the git namespace against publicKeyLine.
func verifySignature(t *testing.T, publicKeyLine string, payload []byte, sig string) {
	t.Helper()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available:", err)
	}
	dir := t.TempDir()

	sigPath := filepath.Join(dir, "payload.sig")
	if err := os.WriteFile(sigPath, []byte(sig), 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}

	// Build an allowed-signers file from the public key the server served.
	fields := strings.Fields(publicKeyLine)
	if len(fields) < 2 {
		t.Fatalf("public key line %q is malformed", publicKeyLine)
	}
	allowed := filepath.Join(dir, "allowed_signers")
	if err := os.WriteFile(allowed, []byte("signer-test "+fields[0]+" "+fields[1]+"\n"), 0o600); err != nil {
		t.Fatalf("write allowed signers: %v", err)
	}

	cmd := exec.Command(keygen, "-Y", "verify", "-n", "git", "-f", allowed, "-I", "signer-test", "-s", sigPath)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y verify rejected signature: %v: %s", err, out)
	}
}
