package server_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	cfg := server.HandlerConfig{
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
		`SIGNER_URL=$base`,
		`SIGNER_PUBLIC_KEY='` + strings.TrimSpace(publicKey) + `'`,
		`SIGNER_COMMITTER_NAME='Jane Dev'`,
		`SIGNER_COMMITTER_EMAIL='jane@example.com'`,
		`curl -fsSL "$base/v1/client/git-remote-sign-linux-amd64" -o "$tmp/git-remote-sign"`,
		`curl -fsSL "$base/v1/client/git-remote-sign-linux-arm64" -o "$tmp/git-remote-sign"`,
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
	cfg := server.HandlerConfig{
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
	cfg := server.HandlerConfig{
		Committer: server.Committer{Name: "Jane Dev", Email: "jane@example.com"},
		DistDir:   t.TempDir(),
		SignerURL: "http://signer.internal:9000/",
	}
	ts := newTestHTTPServer(t, signer, publicKey, cfg)

	_, body := get(t, ts.URL+"/install.sh")
	if !strings.Contains(body, `base='http://signer.internal:9000'`) {
		t.Fatalf("install.sh ignores SIGNER_URL (trailing slash must be stripped)\nscript:\n%s", body)
	}
}

func TestInstallScriptUnavailableWithoutKeyOrDist(t *testing.T) {
	signer, publicKey := newSigner(t)

	// Ready but no dist directory: the script would 404 on its own downloads.
	ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{})
	resp, body := get(t, ts.URL+"/install.sh")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /install.sh (no dist) status = %d, want 503: %s", resp.StatusCode, body)
	}

	// Dist configured but server not ready: no public key to render.
	ts = newTestHTTPServer(t, nil, "", server.HandlerConfig{DistDir: t.TempDir()})
	resp, body = get(t, ts.URL+"/install.sh")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("GET /install.sh (not ready) status = %d, want 503: %s", resp.StatusCode, body)
	}
}

// curlRegexp matches every download command in the rendered bootstrap script.
var curlRegexp = regexp.MustCompile(`curl -fsSL (\S+)`)

// TestInstallScriptDownloadsOnlyWhitelistedArtifacts decodes the client file
// whitelist indirectly from the rendered bootstrap script: every curl -fsSL
// target must be a "$base/v1/client/<name>" download, the resolved set must be
// exactly the whitelisted artifacts (nothing outside the whitelist is curled,
// nothing whitelisted is skipped), and the server must serve each curled name.
// The 404 side of the whitelist lives in TestClientFileRefused.
func TestInstallScriptDownloadsOnlyWhitelistedArtifacts(t *testing.T) {
	signer, publicKey := newSigner(t)
	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": "fake-amd64-binary",
		"git-remote-sign-linux-arm64": "fake-arm64-binary",
		"install-client.sh":           "#!/bin/sh\necho installer\n",
	})
	ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{DistDir: dist})

	_, script := get(t, ts.URL+"/install.sh")

	curled := map[string]bool{}
	for _, m := range curlRegexp.FindAllStringSubmatch(script, -1) {
		const prefix = `"$base/v1/client/`
		name, ok := strings.CutPrefix(m[1], prefix)
		if !ok {
			t.Fatalf("install.sh curls outside /v1/client/: %s", m[0])
		}
		curled[strings.TrimSuffix(name, `"`)] = true
	}

	whitelist := map[string]bool{
		"git-remote-sign-linux-amd64": true,
		"git-remote-sign-linux-arm64": true,
		"install-client.sh":           true,
	}
	for name := range curled {
		if !whitelist[name] {
			t.Fatalf("install.sh downloads non-whitelisted client file %q\nscript:\n%s", name, script)
		}
	}
	for name := range whitelist {
		if !curled[name] {
			t.Fatalf("install.sh never downloads whitelisted client file %q\nscript:\n%s", name, script)
		}
	}

	// The dist fixture holds every whitelisted artifact, so each curled name
	// must resolve to a served file: script and endpoint agree on the set.
	for name := range curled {
		resp, body := get(t, ts.URL+"/v1/client/"+name)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/client/%s status = %d, want 200: %s", name, resp.StatusCode, body)
		}
	}
}

func TestClientFileServed(t *testing.T) {
	signer, publicKey := newSigner(t)
	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": "fake-amd64-binary",
		"install-client.sh":           "#!/bin/sh\necho installer\n",
	})
	ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{DistDir: dist})

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
			ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{DistDir: tc.dist})
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
	ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{})

	_, body := get(t, ts.URL+"/")
	rendered := renderBody(body)
	if !strings.Contains(rendered, "curl -fsSL https://git-signer.int.exe.xyz/install.sh | sh") {
		t.Fatalf("landing page does not show the client bootstrap command\nbody: %s", rendered)
	}
}
