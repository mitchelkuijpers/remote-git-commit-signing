package server_test

import (
	"context"
	"html"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// renderBody returns the page body as a browser would render it: html/template
// escapes "+", "/", and other characters as numeric entities in text context, so
// assertions about visible content compare against the unescaped text.
func renderBody(body string) string {
	return html.UnescapeString(body)
}

// newSignerForKey builds a real signer plus derived public key over an existing
// throwaway keypair, so the test also knows the .pub path for fingerprinting.
func newSignerForKey(t *testing.T, keyPath string) (signing.Signer, string) {
	t.Helper()
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

// sshKeygenFingerprint derives the SHA256 fingerprint of a public key file by
// asking stock ssh-keygen, an independent source of truth for the fingerprint
// the page must display.
func sshKeygenFingerprint(t *testing.T, publicKeyPath string) string {
	t.Helper()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available:", err)
	}
	out, err := exec.Command(keygen, "-l", "-E", "sha256", "-f", publicKeyPath).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen -l: %v: %s", err, out)
	}
	for _, field := range strings.Fields(string(out)) {
		if strings.HasPrefix(field, "SHA256:") {
			return field
		}
	}
	t.Fatalf("ssh-keygen -l output has no SHA256 fingerprint: %q", out)
	return ""
}

func TestLandingPageShowsPublicKeyAndFingerprint(t *testing.T) {
	keyPath := newTestKey(t)
	signer, publicKey := newSignerForKey(t, keyPath)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200: %s", resp.StatusCode, body)
	}

	// The full public key line, not a truncated or reformatted variant.
	rendered := renderBody(body)
	if !strings.Contains(rendered, publicKey) {
		t.Fatalf("GET / body does not contain the full public key line\nbody: %s\nkey:  %s", body, publicKey)
	}

	want := sshKeygenFingerprint(t, filepath.Clean(keyPath)+".pub")
	if !strings.Contains(rendered, want) {
		t.Fatalf("GET / body does not contain fingerprint %q: %s", want, body)
	}
}

func TestLandingPageContentTypeIsHTML(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, body := get(t, ts.URL+"/")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET / Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(body, "<html") && !strings.Contains(body, "<!DOCTYPE") {
		t.Fatalf("GET / body is not HTML: %s", body)
	}
}

func TestLandingPageRendersNoConfigValues(t *testing.T) {
	keyPath := newTestKey(t)
	signer, publicKey := newSignerForKey(t, keyPath)
	const port = 49152
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{
		KeyPath:        keyPath,
		Port:           port,
		CommitterName:  "Secret Committer",
		CommitterEmail: "secret-committer@example.com",
	})

	_, body := get(t, ts.URL+"/")
	rendered := renderBody(body)
	for _, secret := range []string{keyPath, "Secret Committer", "secret-committer@example.com", "PRIVATE KEY"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("GET / leaked configuration value %q: %s", secret, body)
		}
	}
	if strings.Contains(rendered, "49152") {
		t.Fatalf("GET / leaked the configured port: %s", body)
	}
}

func TestLandingPageIncludesGitLabInstructions(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	_, body := get(t, ts.URL+"/")
	for _, want := range []string{"GitLab", "SSH Keys", "Signing", "verify-commit"} {
		if !strings.Contains(renderBody(body), want) {
			t.Fatalf("GET / body is missing %q: %s", want, body)
		}
	}
}

func TestLandingPageNotServedWithoutKey(t *testing.T) {
	ts := newTestHTTPServer(t, nil, "", server.Config{})

	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET / without key status = %d, want 503: %s", resp.StatusCode, body)
	}
}

func TestLandingPageEscapesPublicKeyComment(t *testing.T) {
	signer, publicKey := newSigner(t)
	hostile := publicKey + ` <script>alert("x")</script>`
	ts := newTestHTTPServer(t, signer, hostile, server.Config{})

	_, body := get(t, ts.URL+"/")
	if strings.Contains(body, "<script>") {
		t.Fatalf("GET / did not escape the public key comment: %s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("GET / body is missing the escaped comment: %s", body)
	}
}

func TestLandingPageHasNoJavaScriptOrExternalAssets(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	_, body := get(t, ts.URL+"/")
	for _, banned := range []string{"<script", "javascript:", "src=", `href="http`, "href='http"} {
		if strings.Contains(body, banned) {
			t.Fatalf("GET / body references JavaScript or an external asset (%q): %s", banned, body)
		}
	}
}

func TestLandingPageFailsCleanlyOnMalformedKey(t *testing.T) {
	signer, _ := newSigner(t)
	ts := newTestHTTPServer(t, signer, "not-a-public-key", server.Config{})

	resp, body := get(t, ts.URL+"/")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET / with malformed key status = %d, want 500: %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "not-a-public-key") {
		t.Fatalf("GET / echoed the malformed key: %s", body)
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, _ := get(t, ts.URL+"/does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /does-not-exist status = %d, want 404 (landing page must not shadow it)", resp.StatusCode)
	}
}
