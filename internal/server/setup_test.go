package server_test

import (
	"net"
	"net/http"
	"net/http/httptest"
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

// runRenderedScript executes a rendered bootstrap script with `sh -` in a fresh
// temp working directory and returns that directory (also the script's cwd, for
// relative marker files) plus the combined output and error. `uname` is stubbed
// first on PATH to report machine, so the test fixes the architecture the script
// sees on every host instead of skipping on the rest. It skips when sh or curl
// is unavailable.
func runRenderedScript(t *testing.T, script, machine string) (string, []byte, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available:", err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available:", err)
	}
	work := t.TempDir()
	binDir := filepath.Join(work, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("create stub bin dir: %v", err)
	}
	stub := "#!/bin/sh\nprintf '%s\\n' '" + machine + "'\n"
	if err := os.WriteFile(filepath.Join(binDir, "uname"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write uname stub: %v", err)
	}
	cmd := exec.Command(sh, "-")
	cmd.Stdin = strings.NewReader(script)
	cmd.Dir = work
	cmd.Env = append(os.Environ(),
		"HOME="+work,
		"TMPDIR="+work,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	return work, out, err
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
		`x86_64) arch=amd64 ;;`,
		`aarch64 | arm64) arch=arm64 ;;`,
		`unsupported architecture`,
		`curl -fsSL "$base/v1/client/git-remote-sign-linux-$arch" -o "$tmp/git-remote-sign"`,
		`chmod 0755 "$tmp/git-remote-sign"`,
		`"$base/v1/client/install-client.sh"`,
		`sh "$tmp/install-client.sh"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("install.sh missing %q\nscript:\n%s", want, body)
		}
	}

	shSyntaxCheck(t, body)
}

// fakeClientBinaries are the distinct per-architecture client binaries the
// tests serve: distinct bytes make an architecture mix-up observable.
var fakeClientBinaries = map[string]string{
	"git-remote-sign-linux-amd64": "fake-amd64-binary\n",
	"git-remote-sign-linux-arm64": "fake-arm64-binary\n",
}

// TestInstallScriptInstallsBinaryForStubbedArch executes the rendered bootstrap
// end to end against a stub installer and asserts its externally observable
// effects: with uname -m stubbed to x86_64, exactly the amd64 binary is
// downloaded, it arrives executable, and the installer runs to completion.
// Stubbing uname keeps this regression guard running on every host instead of
// skipping on the ones the signer does not build for; the stub stands in for
// deploy/install-client.sh at the boundary and fails loudly if the bootstrap
// hands it a non-executable binary or the other architecture's.
func TestInstallScriptInstallsBinaryForStubbedArch(t *testing.T) {
	const (
		machine = "x86_64"
		served  = "git-remote-sign-linux-amd64"
	)
	installer := strings.Join([]string{
		"#!/bin/sh",
		"set -eu",
		`if [ ! -x "$GIT_REMOTE_SIGNER_BIN" ]; then echo "stub: not executable: $GIT_REMOTE_SIGNER_BIN" >&2; exit 1; fi`,
		`if [ "$(cat "$GIT_REMOTE_SIGNER_BIN")" != '` + strings.TrimSuffix(fakeClientBinaries[served], "\n") + `' ]; then echo "stub: wrong binary for $(uname -m)" >&2; exit 1; fi`,
		`printf 'installed\n' > installer-ran`,
		"",
	}, "\n")

	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": fakeClientBinaries["git-remote-sign-linux-amd64"],
		"git-remote-sign-linux-arm64": fakeClientBinaries["git-remote-sign-linux-arm64"],
		"install-client.sh":           installer,
	})

	// The rendered script downloads from the signer's own base URL, so the test
	// must know the address before the server starts: reserve the listener,
	// bake its URL into the config, then hand the listener to httptest.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listener: %v", err)
	}
	signer, publicKey := newSigner(t)
	ts := httptest.NewUnstartedServer(server.New(signer, publicKey, server.HandlerConfig{
		Committer: server.Committer{Name: "Jane Dev", Email: "jane@example.com"},
		SignerURL: "http://" + ln.Addr().String(),
		DistDir:   dist,
	}, discardLogger()))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	_, script := get(t, ts.URL+"/install.sh")

	work, out, err := runRenderedScript(t, script, machine)
	if err != nil {
		t.Fatalf("rendered install.sh failed on stubbed %s: %v\noutput:\n%s\nscript:\n%s", machine, err, out, script)
	}
	if _, err := os.Stat(filepath.Join(work, "installer-ran")); err != nil {
		t.Fatalf("stub installer did not run to completion: %v\noutput:\n%s\nscript:\n%s", err, out, script)
	}
}

// TestInstallScriptFailsClosedOnUnservedArchitecture asserts that an
// architecture with no served binary aborts loudly instead of silently
// installing nothing or a wrong variant.
func TestInstallScriptFailsClosedOnUnservedArchitecture(t *testing.T) {
	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": fakeClientBinaries["git-remote-sign-linux-amd64"],
		"git-remote-sign-linux-arm64": fakeClientBinaries["git-remote-sign-linux-arm64"],
		"install-client.sh":           "#!/bin/sh\nexit 0\n",
	})
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{DistDir: dist})

	_, script := get(t, ts.URL+"/install.sh")

	// A stubbed uname reports an architecture the signer does not serve,
	// whatever the test host is.
	_, out, err := runRenderedScript(t, script, "riscv64")
	if err == nil {
		t.Fatalf("install.sh succeeded on an unserved architecture\noutput:\n%s\nscript:\n%s", out, script)
	}
	if !strings.Contains(string(out), "unsupported architecture") {
		t.Fatalf("install.sh did not explain the unsupported architecture\noutput:\n%s\nscript:\n%s", out, script)
	}
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

// archRegexp matches every architecture the bootstrap's uname -m dispatch can
// select.
var archRegexp = regexp.MustCompile(`arch=([A-Za-z0-9_]+)`)

// TestInstallScriptDownloadsOnlyWhitelistedArtifacts decodes the client file
// whitelist indirectly from the rendered bootstrap script: every curl -fsSL
// target must be a "$base/v1/client/<name>" download, the binary target's arch
// variable must resolve to a whitelisted variant for every architecture the
// script selects, the resolved set must be exactly the whitelisted artifacts
// (nothing outside the whitelist is curled, nothing whitelisted is
// unreachable), and the server must serve each reachable name. The 404 side of
// the whitelist lives in TestClientFileRefused.
func TestInstallScriptDownloadsOnlyWhitelistedArtifacts(t *testing.T) {
	signer, publicKey := newSigner(t)
	dist := distDirWith(t, map[string]string{
		"git-remote-sign-linux-amd64": "fake-amd64-binary",
		"git-remote-sign-linux-arm64": "fake-arm64-binary",
		"install-client.sh":           "#!/bin/sh\necho installer\n",
	})
	ts := newTestHTTPServer(t, signer, publicKey, server.HandlerConfig{DistDir: dist})

	_, script := get(t, ts.URL+"/install.sh")

	arches := archRegexp.FindAllStringSubmatch(script, -1)
	if len(arches) == 0 {
		t.Fatalf("install.sh has no uname -m dispatch\nscript:\n%s", script)
	}

	// Resolve every download for each architecture the script can select: the
	// binary target is built from the arch variable, the rest are literal.
	resolved := map[string]bool{}
	for _, m := range curlRegexp.FindAllStringSubmatch(script, -1) {
		const prefix = `"$base/v1/client/`
		name, ok := strings.CutPrefix(m[1], prefix)
		if !ok {
			t.Fatalf("install.sh curls outside /v1/client/: %s", m[0])
		}
		name = strings.TrimSuffix(name, `"`)
		if strings.Contains(name, "$arch") {
			for _, arch := range arches {
				resolved[strings.ReplaceAll(name, "$arch", arch[1])] = true
			}
			continue
		}
		resolved[name] = true
	}

	whitelist := map[string]bool{
		"git-remote-sign-linux-amd64": true,
		"git-remote-sign-linux-arm64": true,
		"install-client.sh":           true,
	}
	for name := range resolved {
		if !whitelist[name] {
			t.Fatalf("install.sh downloads non-whitelisted client file %q\nscript:\n%s", name, script)
		}
	}
	for name := range whitelist {
		if !resolved[name] {
			t.Fatalf("install.sh never downloads whitelisted client file %q\nscript:\n%s", name, script)
		}
	}

	// The dist fixture holds every whitelisted artifact, so each reachable name
	// must resolve to a served file: script and endpoint agree on the set.
	for name := range whitelist {
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
