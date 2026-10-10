package server_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
)

// shSyntaxCheck asserts the rendered bootstrap script parses as POSIX sh.
func shSyntaxCheck(t *testing.T, script string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available:", err)
	}
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rendered install.sh fails `sh -n`: %v: %s\nscript:\n%s", err, out, script)
	}
}

// distDirWith creates a temporary dist directory containing the given files.
func distDirWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatalf("write dist file: %v", err)
		}
	}
	return dir
}

func TestInstallScriptRendersPinnedConfig(t *testing.T) {
	signer, publicKey := newSigner(t)
	cfg := server.Config{
		Committer: server.Committer{Name: "Jane Dev", Email: "jane@example.com"},
		DistDir:   t.TempDir(),
	}
	ts := newTestHTTPServer(t, signer, publicKey, cfg)

	resp, body := get(t, ts.URL+"/install.sh")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /install.sh status = %d, want 200: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Fatalf("GET /install.sh Content-Type = %q, want text/x-shellscript", ct)
	}

	// The script must carry every pinned value the installer needs, so the
	// agent VM operator runs zero configuration by hand.
	for _, want := range []string{
		`base='https://git-signer.int.exe.xyz'`,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY='" + strings.TrimSpace(publicKey) + "'",
		`GIT_SIGNER_COMMITTER_NAME='Jane Dev'`,
		`GIT_SIGNER_COMMITTER_EMAIL='jane@example.com'`,
		`"$base/v1/client/git-remote-sign-linux-$arch"`,
		`"$base/v1/client/install-client.sh"`,
		`sh "$tmp/install-client.sh"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("install.sh missing %q\nscript:\n%s", want, body)
		}
	}

	shSyntaxCheck(t, body)
}

func TestInstallScriptShellQuotesValues(t *testing.T) {
	signer, publicKey := newSigner(t)
	cfg := server.Config{
		Committer: server.Committer{Name: "O'Brien", Email: "obrien@example.com"},
		DistDir:   t.TempDir(),
	}
	ts := newTestHTTPServer(t, signer, publicKey, cfg)

	_, body := get(t, ts.URL+"/install.sh")
	if !strings.Contains(body, `'O'\''Brien'`) {
		t.Fatalf("install.sh does not shell-quote the committer name\nscript:\n%s", body)
	}
	shSyntaxCheck(t, body)
}

func TestInstallScriptUsesConfiguredPublicURL(t *testing.T) {
	signer, publicKey := newSigner(t)
	cfg := server.Config{
		Committer: server.Committer{Name: "Jane Dev", Email: "jane@example.com"},
		DistDir:   t.TempDir(),
		PublicURL: "http://signer.internal:9000/",
	}
	ts := newTestHTTPServer(t, signer, publicKey, cfg)

	_, body := get(t, ts.URL+"/install.sh")
	if !strings.Contains(body, `base='http://signer.internal:9000'`) {
		t.Fatalf("install.sh ignores SIGNER_PUBLIC_URL (trailing slash must be stripped)\nscript:\n%s", body)
	}
}

func TestInstallScriptUnavailableWithoutKeyOrDist(t *testing.T) {
	signer, publicKey := newSigner(t)

	// Ready but no dist directory: the script would 404 on its own downloads.
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})
	resp, body := get(t, ts.URL+"/install.sh")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /install.sh (no dist) status = %d, want 503: %s", resp.StatusCode, body)
	}

	// Dist configured but server not ready: no public key to render.
	ts = newTestHTTPServer(t, nil, "", server.Config{DistDir: t.TempDir()})
	resp, body = get(t, ts.URL+"/install.sh")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /install.sh (not ready) status = %d, want 503: %s", resp.StatusCode, body)
	}
}

func TestClientFileServed(t *testing.T) {
	signer, publicKey := newSigner(t)
	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": "fake-amd64-binary",
		"install-client.sh":           "#!/bin/sh\necho installer\n",
	})
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{DistDir: dist})

	resp, body := get(t, ts.URL+"/v1/client/git-remote-sign-linux-amd64")
	if resp.StatusCode != http.StatusOK || body != "fake-amd64-binary" {
		t.Fatalf("GET client binary = %d %q, want 200 with file content", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/octet-stream") {
		t.Fatalf("client binary Content-Type = %q, want application/octet-stream", ct)
	}

	resp, body = get(t, ts.URL+"/v1/client/install-client.sh")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "echo installer") {
		t.Fatalf("GET install-client.sh = %d %q, want 200 with file content", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Fatalf("install-client.sh Content-Type = %q, want text/x-shellscript", ct)
	}
}

func TestClientFileRefused(t *testing.T) {
	signer, publicKey := newSigner(t)
	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": "fake-amd64-binary",
		"secret.txt":                  "must not leak",
	})

	for _, tc := range []struct {
		name string
		dist string
		path string
	}{
		{"not in whitelist", dist, "/v1/client/secret.txt"},
		{"traversal name", dist, "/v1/client/.."},
		{"unknown named file", dist, "/v1/client/git-remote-sign-linux-arm64"},
		{"dist disabled", "", "/v1/client/git-remote-sign-linux-amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestHTTPServer(t, signer, publicKey, server.Config{DistDir: tc.dist})
			resp, body := get(t, ts.URL+tc.path)
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("GET %s status = %d, want 404: %s", tc.path, resp.StatusCode, body)
			}
			if strings.Contains(body, "must not leak") {
				t.Fatalf("GET %s leaked non-whitelisted file content", tc.path)
			}
		})
	}
}

func TestLandingPageShowsBootstrapCommand(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	_, body := get(t, ts.URL+"/")
	rendered := renderBody(body)
	if !strings.Contains(rendered, "curl -fsSL https://git-signer.int.exe.xyz/install.sh | sh") {
		t.Fatalf("landing page does not show the client bootstrap command\nbody: %s", rendered)
	}
}
