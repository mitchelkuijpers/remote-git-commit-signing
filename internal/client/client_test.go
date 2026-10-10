package client_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/client"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

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

// publicKeyLine returns the authorized_keys line for a private key.
func publicKeyLine(t *testing.T, keyPath string) string {
	t.Helper()
	line, err := signing.PublicKey(context.Background(), keyPath)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	return line
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newSignServer starts an httptest server whose /v1/sign handler signs the
// request body with a real ssh-keygen signer. The received payload is recorded
// into captured when non-nil.
func newSignServer(t *testing.T, signer signing.Signer, captured *[]byte) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sign" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if captured != nil {
			*captured = body
		}
		sig, err := signer.Sign(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.sshsig")
		w.Write(sig)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newRawSignServer returns whatever body the test provides.
func newRawSignServer(t *testing.T, status int, body string, calls *int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func env(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

// runClient invokes client.Run with empty stdin and discarded stdout, as a
// signing invocation would be driven.
func runClient(argv []string, getenv func(string) string, stderr io.Writer) int {
	return client.Run(argv, getenv, strings.NewReader(""), io.Discard, stderr)
}

// signArgv builds Git's signing invocation for the given key and buffer.
func signArgv(keyFile, bufferFile string) []string {
	return []string{"git-remote-sign", "-Y", "sign", "-n", "git", "-f", keyFile, bufferFile}
}

// expectNoSig fails if a .sig file exists for bufferFile.
func expectNoSig(t *testing.T, bufferFile string) {
	t.Helper()
	if _, err := os.Stat(bufferFile + ".sig"); !os.IsNotExist(err) {
		t.Fatalf("%s.sig exists after failure (stat err=%v)", bufferFile, err)
	}
}

// independentlyVerifies confirms sig is a valid "git" signature over payload
// for pubLine, using ssh-keygen directly (an independent source of truth).
func independentlyVerifies(t *testing.T, pubLine string, payload, sig []byte) bool {
	t.Helper()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available:", err)
	}
	dir := t.TempDir()
	fields := strings.Fields(pubLine)
	allowed := "git-remote-sign " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(filepath.Join(dir, "allowed"), []byte(allowed), 0o600); err != nil {
		t.Fatalf("write allowed signers: %v", err)
	}
	sigPath := filepath.Join(dir, "sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatalf("write sig: %v", err)
	}
	cmd := exec.Command(keygen, "-Y", "verify", "-n", "git",
		"-f", filepath.Join(dir, "allowed"), "-I", "git-remote-sign", "-s", sigPath)
	cmd.Stdin = bytes.NewReader(payload)
	return cmd.Run() == nil
}

func TestSignHappyPathWithLiteralPinnedKey(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	var captured []byte
	ts := newSignServer(t, signer, &captured)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	payload := []byte("tree 0000\n\ncommit message\n")
	writeFile(t, bufferFile, string(payload))

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        ts.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)

	if code != 0 {
		t.Fatalf("Run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !bytes.Equal(captured, payload) {
		t.Fatalf("server received %q, want the exact payload %q", captured, payload)
	}
	sig, err := os.ReadFile(bufferFile + ".sig")
	if err != nil {
		t.Fatalf("read .sig: %v", err)
	}
	if !independentlyVerifies(t, pubLine, payload, sig) {
		t.Fatalf("written .sig does not verify independently")
	}
}

func TestSignHappyPathWithPinnedKeyFilePath(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	ts := newSignServer(t, signer, nil)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	pinnedFile := filepath.Join(dir, "pinned.pub")
	writeFile(t, pinnedFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        ts.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pinnedFile,
	}), &stderr)
	if code != 0 {
		t.Fatalf("Run exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(bufferFile + ".sig"); err != nil {
		t.Fatalf("expected .sig written: %v", err)
	}
}

func TestSignRejectsWrongKeyFile(t *testing.T) {
	signerKey := newTestKey(t)
	otherKey := newTestKey(t)
	calls := 0
	raw := newRawSignServer(t, http.StatusOK, "irrelevant", &calls)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, publicKeyLine(t, otherKey)+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        raw.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": publicKeyLine(t, signerKey),
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for key mismatch")
	}
	if calls != 0 {
		t.Fatalf("signer was contacted %d times despite key mismatch", calls)
	}
	if !strings.Contains(stderr.String(), "does not match the pinned") {
		t.Fatalf("stderr = %q, want a pinned-key mismatch message", stderr.String())
	}
	expectNoSig(t, bufferFile)
}

func TestSignRejectsUnreachableServer(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)

	// Start and immediately close a server to get a definitely-closed address.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        deadURL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for unreachable server")
	}
	expectNoSig(t, bufferFile)
}

func TestSignRefusesRedirect(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)

	// A redirecting edge (classically http:// -> https:// on exe.dev) must not
	// be followed: Go would downgrade the sign POST to a GET and report an
	// opaque 405. The client must name the redirect instead.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", "https://git-signer.int.exe.xyz/v1/sign")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        ts.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for a redirecting signer")
	}
	if !strings.Contains(stderr.String(), "redirected") {
		t.Fatalf("stderr does not name the redirect: %q", stderr.String())
	}
	expectNoSig(t, bufferFile)
}

func TestSignRejectsGarbageSignatureAndRemovesStaleSig(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	raw := newRawSignServer(t, http.StatusOK, "not a signature at all", nil)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")
	// Simulate a stale partial signature from an earlier aborted run.
	writeFile(t, bufferFile+".sig", "stale partial")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        raw.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for garbage signature")
	}
	expectNoSig(t, bufferFile)
}

func TestSignRejectsSignatureFromUnpinnedKey(t *testing.T) {
	// The server signs with a different key than the pinned one; the signature
	// is valid over the payload but must not be accepted.
	pinnedKey := newTestKey(t)
	otherKey := newTestKey(t)
	otherSigner, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: otherKey})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	ts := newSignServer(t, otherSigner, nil)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, publicKeyLine(t, pinnedKey)+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        ts.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": publicKeyLine(t, pinnedKey),
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for signature from unpinned key")
	}
	expectNoSig(t, bufferFile)
}

func TestSignRejectsOversizedResponse(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	big := strings.Repeat("A", (64<<10)+1)
	raw := newRawSignServer(t, http.StatusOK, big, nil)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        raw.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for oversized response")
	}
	if !strings.Contains(stderr.String(), "exceeds") {
		t.Fatalf("stderr = %q, want a size-cap message", stderr.String())
	}
	expectNoSig(t, bufferFile)
}

func TestSignRejectsNonOKStatus(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	raw := newRawSignServer(t, http.StatusForbidden, "denied", nil)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        raw.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for non-OK status")
	}
	expectNoSig(t, bufferFile)
}

// TestSignSanitizesUntrustedErrorBody covers the cleartext-HTTP threat: a
// misconfigured or hostile signer could echo the payload or emit terminal
// escapes in an error body. The client must report the status plus a sanitized
// summary, never the raw bytes.
func TestSignSanitizesUntrustedErrorBody(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	// First line carries terminal escapes and control bytes plus a long run, so
	// the summary is both filtered and capped; the second line must be dropped.
	hostile := "\x1b[2Jsigner-said\x00\x07no\r" + strings.Repeat("A", 500) + "\nsecond line\n"
	raw := newRawSignServer(t, http.StatusForbidden, hostile, nil)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        raw.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
	}), &stderr)
	if code == 0 {
		t.Fatal("Run exit = 0, want non-zero for non-OK status")
	}
	out := stderr.String()
	if !strings.Contains(out, "403 Forbidden") {
		t.Fatalf("stderr = %q, want the HTTP status", out)
	}
	if !strings.Contains(out, "signer-said") {
		t.Fatalf("stderr = %q, want the sanitized first-line summary", out)
	}
	for _, banned := range []string{"\x1b", "\x00", "\x07", "\r", "second line"} {
		if strings.Contains(out, banned) {
			t.Fatalf("stderr echoed untrusted bytes %q: %q", banned, out)
		}
	}
	if strings.Contains(out, strings.Repeat("A", 201)) {
		t.Fatalf("stderr summary is not length-capped: %q", out)
	}
	expectNoSig(t, bufferFile)
}

func TestUnknownOperationsFailLoudly(t *testing.T) {
	// Signing and the three verify operations are the only supported -Y
	// operations. Anything else must fail loudly rather than be mishandled.
	for _, op := range []string{"frobnicate", "list-keys"} {
		t.Run(op, func(t *testing.T) {
			var stderr bytes.Buffer
			code := runClient([]string{"git-remote-sign", "-Y", op, "-n", "git"}, env(nil), &stderr)
			if code == 0 {
				t.Fatalf("op %q: exit = 0, want non-zero", op)
			}
			if !strings.Contains(stderr.String(), "not implemented") {
				t.Fatalf("op %q: stderr = %q, want an explicit not-implemented error", op, stderr.String())
			}
		})
	}
}

func TestOperationIsPositionalNotSubstring(t *testing.T) {
	// "-Y" must be argv[1]; anything else is a loud error rather than a sign.
	var stderr bytes.Buffer
	code := runClient([]string{"git-remote-sign", "sign", "-Y", "-n", "git"}, env(nil), &stderr)
	if code == 0 {
		t.Fatal("exit = 0, want non-zero when -Y is not positional")
	}

	// A verification-looking argument in another position must not be mistaken
	// for a sign request.
	stderr.Reset()
	code = runClient([]string{"git-remote-sign", "-Y", "find-principals"}, env(nil), &stderr)
	if code == 0 {
		t.Fatal("exit = 0, want non-zero for find-principals")
	}
}

func TestSignRequiresGitNamespace(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	calls := 0
	raw := newRawSignServer(t, http.StatusOK, "irrelevant", &calls)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient([]string{"git-remote-sign", "-Y", "sign", "-n", "other", "-f", keyFile, bufferFile},
		env(map[string]string{
			"GIT_REMOTE_SIGNER_URL":        raw.URL,
			"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
		}), &stderr)
	if code == 0 {
		t.Fatal("exit = 0, want non-zero for non-git namespace")
	}
	if calls != 0 {
		t.Fatalf("signer contacted %d times for a non-git namespace", calls)
	}
	expectNoSig(t, bufferFile)
}

func TestSignRequiresConfiguration(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	cases := map[string]map[string]string{
		"missing url":    {"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine},
		"missing pubkey": {"GIT_REMOTE_SIGNER_URL": "http://127.0.0.1:1"},
		"bad url":        {"GIT_REMOTE_SIGNER_URL": "ftp://x", "GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine},
		"bad timeout":    {"GIT_REMOTE_SIGNER_URL": "http://127.0.0.1:1", "GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine, "GIT_REMOTE_SIGN_TIMEOUT": "nonsense"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := runClient(signArgv(keyFile, bufferFile), env(e), &stderr); code == 0 {
				t.Fatalf("exit = 0, want non-zero; stderr=%s", stderr.String())
			}
			expectNoSig(t, bufferFile)
		})
	}
}

func TestSignTimeoutIsApplied(t *testing.T) {
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)

	// Server that stalls longer than the configured timeout.
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-release
	}))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { close(release) })

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing.pub")
	writeFile(t, keyFile, pubLine+"\n")
	bufferFile := filepath.Join(dir, "buffer")
	writeFile(t, bufferFile, "payload\n")

	var stderr bytes.Buffer
	code := runClient(signArgv(keyFile, bufferFile), env(map[string]string{
		"GIT_REMOTE_SIGNER_URL":        ts.URL,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": pubLine,
		"GIT_REMOTE_SIGN_TIMEOUT":      "50ms",
	}), &stderr)
	if code == 0 {
		t.Fatal("exit = 0, want non-zero on timeout")
	}
	expectNoSig(t, bufferFile)
}
