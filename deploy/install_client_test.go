package deploy_test

// Tests for deploy/install-client.sh, the one-command agent-VM provisioner.
//
// The seam is the script's end-to-end behaviour: it is executed for real with a
// sandboxed HOME/XDG_CONFIG_HOME/TMPDIR and a real signer server, and the tests
// assert on the resulting state (files, modes, git config, self-test outcome).
// The exe.dev platform is simulated the same way the client's own end-to-end
// tests do it: an HTTP proxy in front of the real server that stamps the
// verified X-Exedev-Source-Vm identity, exactly as production plumbing does.
// The client is never weakened and no identity header is ever sent by it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

const (
	installVMIdentity = "install-test-vm"
	installName       = "Install Test Agent"
	installEmail      = "install@example.com"
	installerPath     = "deploy/install-client.sh"
)

// --- environment helpers -----------------------------------------------------

// moduleRoot returns the module root (the directory holding go.mod).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// dropEnv are variables the host may set that would leak into (or corrupt) the
// sandboxed installer run.
var dropEnv = map[string]bool{
	"HOME": true, "XDG_CONFIG_HOME": true, "TMPDIR": true,
	"GIT_CONFIG_GLOBAL": true, "GIT_CONFIG_SYSTEM": true,
	"SIGNER_URL": true, "SIGNER_PUBLIC_KEY": true,
	"GIT_REMOTE_SIGNER_BIN": true, "GIT_REMOTE_SIGNER_RELEASE_VERSION": true,
	"GIT_REMOTE_SIGNER_DOWNLOAD_BASE": true, "GIT_REMOTE_SIGNER_INSTALL_DIR": true,
	"GIT_REMOTE_SIGNER_CONFIG_DIR": true, "GIT_REMOTE_SIGNER_PROFILE": true,
	"SIGNER_TIMEOUT":        true,
	"SIGNER_COMMITTER_NAME": true, "SIGNER_COMMITTER_EMAIL": true,
}

// mergeEnv starts from the process environment, drops host state that could
// leak in, and applies overrides.
func mergeEnv(overrides map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		if dropEnv[kv[:eq]] {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// installerEnv builds the environment for one installer run in sandbox.
func installerEnv(sandbox string, overrides map[string]string) map[string]string {
	env := map[string]string{
		"HOME":                sandbox,
		"XDG_CONFIG_HOME":     filepath.Join(sandbox, ".config"),
		"TMPDIR":              filepath.Join(sandbox, "tmp"),
		"GIT_CONFIG_GLOBAL":   filepath.Join(sandbox, ".gitconfig"),
		"GIT_CONFIG_NOSYSTEM": "1",
	}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// newSandbox creates an isolated HOME with a TMPDIR the installer can use.
func newSandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatalf("create sandbox tmp: %v", err)
	}
	return dir
}

// result is a finished subprocess.
type result struct {
	exit           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit=%d stdout=%q stderr=%q", r.exit, r.stdout, r.stderr)
}

// run executes name with args, using the given environment overrides.
func run(t *testing.T, dir string, env map[string]string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = mergeEnv(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", name, args, err)
		}
	}
	return result{exit: code, stdout: stdout.String(), stderr: stderr.String()}
}

// runInstaller runs the provisioning script for real.
func runInstaller(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	return run(t, moduleRoot(t), env, "sh", append([]string{installerPath}, args...)...)
}

// gitGlobal runs git with the sandbox's global config in effect.
func gitGlobal(t *testing.T, sandbox string, args ...string) string {
	t.Helper()
	r := run(t, sandbox, installerEnv(sandbox, nil), "git", args...)
	if r.exit != 0 {
		t.Fatalf("git %s: %s", strings.Join(args, " "), r)
	}
	return r.stdout
}

// --- fixture helpers ---------------------------------------------------------

// requireTools skips when the tools the installer needs are unavailable.
func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen", "curl", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
	}
}

// buildClient compiles git-remote-sign and returns its path.
func buildClient(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available:", err)
	}
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", dir, "./cmd/git-remote-sign")
	cmd.Dir = moduleRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	return filepath.Join(dir, "git-remote-sign")
}

// newTestKey generates a throwaway ED25519 keypair and returns the private path.
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

// writePub derives keyPath's public key into a file and returns the path.
func writePub(t *testing.T, keyPath string) string {
	t.Helper()
	line, err := signing.PublicKey(context.Background(), keyPath)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "signing.pub")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return path
}

// signerFixture is a running signer server plus the pinned key material.
type signerFixture struct {
	url     string
	pubLine string
	pubPath string
}

// startSigner starts the real signer server behind a proxy that stamps the
// verified VM identity, exactly as the exe.dev platform does in production.
func startSigner(t *testing.T) *signerFixture {
	t.Helper()
	keyPath := newTestKey(t)
	pubLine, err := signing.PublicKey(context.Background(), keyPath)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	pubPath := filepath.Join(t.TempDir(), "signing.pub")
	if err := os.WriteFile(pubPath, []byte(pubLine+"\n"), 0o644); err != nil {
		t.Fatalf("write public key: %v", err)
	}

	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	cfg := server.HandlerConfig{
		Committer:  server.Committer{Name: installName, Email: installEmail},
		Allowlist:  server.Allowlist{installVMIdentity},
		RatePerMin: 6000,
		RateBurst:  1000,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(signer, pubLine, cfg, logger)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.StampSourceVM(r.Header, installVMIdentity)
		srv.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return &signerFixture{url: ts.URL, pubLine: pubLine, pubPath: pubPath}
}

// happyEnv is the configuration a real agent VM is provisioned with.
func happyEnv(t *testing.T, sandbox string, s *signerFixture, clientBin string) map[string]string {
	t.Helper()
	overrides := map[string]string{
		"SIGNER_URL":             s.url,
		"SIGNER_PUBLIC_KEY":      s.pubPath,
		"SIGNER_COMMITTER_NAME":  installName,
		"SIGNER_COMMITTER_EMAIL": installEmail,
	}
	if clientBin != "" {
		overrides["GIT_REMOTE_SIGNER_BIN"] = clientBin
	}
	return installerEnv(sandbox, overrides)
}

// --- state assertions --------------------------------------------------------

// manifest is a deterministic digest of every file (mode + content hash) and
// directory (mode) under root, used to prove idempotence.
func manifest(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			lines = append(lines, fmt.Sprintf("dir  %04o %s", info.Mode().Perm(), rel))
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		lines = append(lines, fmt.Sprintf("file %04o %s %s", info.Mode().Perm(), hex.EncodeToString(sum[:]), rel))
		return nil
	})
	if err != nil {
		t.Fatalf("manifest %s: %v", root, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// assertMode fails unless path exists with the given permission bits.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

// assertNoPrivateKey fails if any file under root contains private key material.
func assertNoPrivateKey(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Errorf("private key material written to %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan for private keys: %v", err)
	}
}

// assertEmptyDir fails if dir contains any entry.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s not empty after install (self-test left trace): %v", dir, names)
	}
}

// configValue returns the value of key from `git config --global --list`.
func configValue(t *testing.T, listing, key string) string {
	t.Helper()
	for _, line := range strings.Split(listing, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
	}
	return ""
}

// --- tests -------------------------------------------------------------------

// TestInstallClientProvisionsIdempotently is the core acceptance test: one run
// provisions a fresh VM, the self-test passes, re-running changes nothing, and
// no private key material is written.
func TestInstallClientProvisionsIdempotently(t *testing.T) {
	requireTools(t)
	clientBin := buildClient(t)
	s := startSigner(t)
	sandbox := newSandbox(t)
	env := happyEnv(t, sandbox, s, clientBin)

	first := runInstaller(t, env)
	if first.exit != 0 {
		t.Fatalf("installer failed: %s", first)
	}
	if !strings.Contains(first.stderr, "self-test passed") {
		t.Fatalf("installer did not run the signing self-test: %s", first)
	}

	binPath := filepath.Join(sandbox, ".local", "bin", "git-remote-sign")
	pubPath := filepath.Join(sandbox, ".config", "git-remote-signer", "signing.pub")
	allowedPath := filepath.Join(sandbox, ".config", "git-remote-signer", "allowed_signers")
	assertMode(t, binPath, 0o755)
	assertMode(t, pubPath, 0o644)
	assertMode(t, allowedPath, 0o644)
	if got, err := os.ReadFile(pubPath); err != nil || strings.TrimSpace(string(got)) != s.pubLine {
		t.Fatalf("installed public key = %q, err=%v, want %q", got, err, s.pubLine)
	}
	// The allowed-signers file maps the pinned committer email to the pinned
	// key, so local `git verify-commit` works on a fresh VM without manual setup.
	fields := strings.Fields(s.pubLine)
	wantAllowed := installEmail + " " + fields[0] + " " + fields[1] + "\n"
	if got, err := os.ReadFile(allowedPath); err != nil || string(got) != wantAllowed {
		t.Fatalf("installed allowed-signers file = %q, err=%v, want %q", got, err, wantAllowed)
	}
	assertNoPrivateKey(t, sandbox)
	assertEmptyDir(t, filepath.Join(sandbox, "tmp"))

	cfg := gitGlobal(t, sandbox, "config", "--global", "--list")
	// git config --list lowercases variable names, so the allowed-signers key
	// is listed as gpg.ssh.allowedsignersfile.
	for key, want := range map[string]string{
		"gpg.format":                 "ssh",
		"gpg.ssh.program":            binPath,
		"commit.gpgsign":             "true",
		"user.signingkey":            pubPath,
		"gpg.ssh.allowedsignersfile": allowedPath,
		"user.name":                  installName,
		"user.email":                 installEmail,
	} {
		if got := configValue(t, cfg, key); got != want {
			t.Errorf("git config %s = %q, want %q", key, got, want)
		}
	}

	firstManifest := manifest(t, sandbox)
	firstCfg := cfg

	second := runInstaller(t, env)
	if second.exit != 0 {
		t.Fatalf("second installer run failed: %s", second)
	}
	if got := manifest(t, sandbox); got != firstManifest {
		t.Errorf("installer is not idempotent:\n--- first run ---\n%s\n--- second run ---\n%s", firstManifest, got)
	}
	if got := gitGlobal(t, sandbox, "config", "--global", "--list"); got != firstCfg {
		t.Errorf("git config changed on second run:\n--- first ---\n%s\n--- second ---\n%s", firstCfg, got)
	}
}

// TestInstallClientPreservesUnrelatedGitConfig proves the installer only sets
// the signing keys and leaves the rest of the user's git config alone.
func TestInstallClientPreservesUnrelatedGitConfig(t *testing.T) {
	requireTools(t)
	clientBin := buildClient(t)
	s := startSigner(t)
	sandbox := newSandbox(t)

	existing := "[alias]\n\tco = checkout\n[core]\n\tpager = cat\n"
	if err := os.WriteFile(filepath.Join(sandbox, ".gitconfig"), []byte(existing), 0o644); err != nil {
		t.Fatalf("seed gitconfig: %v", err)
	}

	if r := runInstaller(t, happyEnv(t, sandbox, s, clientBin)); r.exit != 0 {
		t.Fatalf("installer failed: %s", r)
	}

	cfg := gitGlobal(t, sandbox, "config", "--global", "--list")
	for key, want := range map[string]string{
		"alias.co":   "checkout",
		"core.pager": "cat",
		"gpg.format": "ssh",
	} {
		if got := configValue(t, cfg, key); got != want {
			t.Errorf("git config %s = %q, want %q", key, got, want)
		}
	}
}

// TestInstallClientFailsWhenSignerUnreachable covers the reachability check.
func TestInstallClientFailsWhenSignerUnreachable(t *testing.T) {
	requireTools(t)
	clientBin := buildClient(t)
	sandbox := newSandbox(t)
	env := installerEnv(sandbox, map[string]string{
		// Nothing listens here, so GET /healthz is refused immediately.
		"SIGNER_URL":             "http://127.0.0.1:1",
		"SIGNER_PUBLIC_KEY":      writePub(t, newTestKey(t)),
		"SIGNER_COMMITTER_NAME":  installName,
		"SIGNER_COMMITTER_EMAIL": installEmail,
		"GIT_REMOTE_SIGNER_BIN":  clientBin,
	})

	r := runInstaller(t, env)
	if r.exit == 0 {
		t.Fatalf("installer succeeded against an unreachable signer: %s", r)
	}
	if !strings.Contains(r.stderr, "not reachable") {
		t.Errorf("error does not name the reachability failure: %s", r)
	}
	if _, err := os.Stat(filepath.Join(sandbox, ".local", "bin", "git-remote-sign")); !os.IsNotExist(err) {
		t.Errorf("binary installed despite a failed reachability check (err=%v)", err)
	}
}

// TestInstallClientFailsOnPinnedKeyMismatch covers the hard cross-check.
func TestInstallClientFailsOnPinnedKeyMismatch(t *testing.T) {
	requireTools(t)
	clientBin := buildClient(t)
	s := startSigner(t)
	sandbox := newSandbox(t)
	// Pin a different key than the server holds.
	env := installerEnv(sandbox, map[string]string{
		"SIGNER_URL":             s.url,
		"SIGNER_PUBLIC_KEY":      writePub(t, newTestKey(t)),
		"SIGNER_COMMITTER_NAME":  installName,
		"SIGNER_COMMITTER_EMAIL": installEmail,
		"GIT_REMOTE_SIGNER_BIN":  clientBin,
	})

	r := runInstaller(t, env)
	if r.exit == 0 {
		t.Fatalf("installer accepted a key the signer does not hold: %s", r)
	}
	if !strings.Contains(r.stderr, "does not match") {
		t.Errorf("error does not name the key mismatch: %s", r)
	}
	if _, err := os.Stat(filepath.Join(sandbox, ".local", "bin", "git-remote-sign")); !os.IsNotExist(err) {
		t.Errorf("binary installed despite a key mismatch (err=%v)", err)
	}
}

// TestInstallClientInstallsVerifiedDownload exercises the release path: the
// artifact is downloaded, checksum-verified, installed and self-tested.
func TestInstallClientInstallsVerifiedDownload(t *testing.T) {
	requireTools(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("release artifacts are only published for linux/darwin")
	}
	clientBin := buildClient(t)
	s := startSigner(t)
	sandbox := newSandbox(t)
	rel := newReleaseServer(t, clientBin, "")

	env := installerEnv(sandbox, map[string]string{
		"SIGNER_URL":                      s.url,
		"SIGNER_PUBLIC_KEY":               s.pubPath,
		"SIGNER_COMMITTER_NAME":           installName,
		"SIGNER_COMMITTER_EMAIL":          installEmail,
		"GIT_REMOTE_SIGNER_DOWNLOAD_BASE": rel.url,
	})

	r := runInstaller(t, env)
	if r.exit != 0 {
		t.Fatalf("installer failed for a verified download: %s", r)
	}
	if !strings.Contains(r.stderr, "checksum verified") {
		t.Errorf("installer did not report checksum verification: %s", r)
	}
	if !strings.Contains(r.stderr, "self-test passed") {
		t.Errorf("downloaded binary did not pass the self-test: %s", r)
	}
	assertMode(t, filepath.Join(sandbox, ".local", "bin", "git-remote-sign"), 0o755)
}

// TestInstallClientRejectsChecksumMismatch proves an unverified download is
// never installed.
func TestInstallClientRejectsChecksumMismatch(t *testing.T) {
	requireTools(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("release artifacts are only published for linux/darwin")
	}
	clientBin := buildClient(t)
	s := startSigner(t)
	sandbox := newSandbox(t)
	rel := newReleaseServer(t, clientBin, strings.Repeat("0", 64))

	env := installerEnv(sandbox, map[string]string{
		"SIGNER_URL":                      s.url,
		"SIGNER_PUBLIC_KEY":               s.pubPath,
		"SIGNER_COMMITTER_NAME":           installName,
		"SIGNER_COMMITTER_EMAIL":          installEmail,
		"GIT_REMOTE_SIGNER_DOWNLOAD_BASE": rel.url,
	})

	r := runInstaller(t, env)
	if r.exit == 0 {
		t.Fatalf("installer installed a download with a bad checksum: %s", r)
	}
	if !strings.Contains(r.stderr, "checksum") {
		t.Errorf("error does not name the checksum failure: %s", r)
	}
	if _, err := os.Stat(filepath.Join(sandbox, ".local", "bin", "git-remote-sign")); !os.IsNotExist(err) {
		t.Errorf("binary installed despite a failed checksum (err=%v)", err)
	}
}

// TestInstallClientMissingConfigFailsClearly checks the required-variable
// errors are actionable.
func TestInstallClientMissingConfigFailsClearly(t *testing.T) {
	requireTools(t)
	sandbox := newSandbox(t)
	r := runInstaller(t, installerEnv(sandbox, nil))
	if r.exit == 0 {
		t.Fatalf("installer succeeded with no configuration: %s", r)
	}
	if !strings.Contains(r.stderr, "SIGNER_URL") || !strings.Contains(r.stderr, "required") {
		t.Errorf("error is not actionable: %s", r)
	}
}

// TestInstallClientMissingCurlFailsClearly uses a fake PATH that has git and
// ssh-keygen but no curl: the installer must abort with a clear message.
func TestInstallClientMissingCurlFailsClearly(t *testing.T) {
	requireTools(t)
	fake := t.TempDir()
	for _, tool := range []string{"git", "ssh-keygen"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available: %v", tool, err)
		}
		if err := os.Symlink(p, filepath.Join(fake, tool)); err != nil {
			t.Fatalf("symlink %s: %v", tool, err)
		}
	}
	sandbox := newSandbox(t)
	env := installerEnv(sandbox, map[string]string{
		"PATH":                   fake,
		"SIGNER_URL":             "http://127.0.0.1:1",
		"SIGNER_PUBLIC_KEY":      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA test@example.com",
		"SIGNER_COMMITTER_NAME":  installName,
		"SIGNER_COMMITTER_EMAIL": installEmail,
	})

	r := runInstaller(t, env)
	if r.exit == 0 {
		t.Fatalf("installer succeeded without curl: %s", r)
	}
	if !strings.Contains(r.stderr, "curl") {
		t.Errorf("error does not name the missing tool: %s", r)
	}
}

// TestInstallClientAcceptsLiteralPinnedKeyAndSkipsSelfTest covers the literal
// pinned-key form and the --skip-selftest flag.
func TestInstallClientAcceptsLiteralPinnedKeyAndSkipsSelfTest(t *testing.T) {
	requireTools(t)
	clientBin := buildClient(t)
	s := startSigner(t)
	sandbox := newSandbox(t)
	env := installerEnv(sandbox, map[string]string{
		"SIGNER_URL":             s.url,
		"SIGNER_PUBLIC_KEY":      s.pubLine, // literal line, not a path
		"SIGNER_COMMITTER_NAME":  installName,
		"SIGNER_COMMITTER_EMAIL": installEmail,
		"GIT_REMOTE_SIGNER_BIN":  clientBin,
	})

	r := runInstaller(t, env, "--skip-selftest")
	if r.exit != 0 {
		t.Fatalf("installer failed for a literal pinned key: %s", r)
	}
	if !strings.Contains(r.stderr, "skipping the signing self-test") {
		t.Errorf("--skip-selftest was not honoured: %s", r)
	}
	pubPath := filepath.Join(sandbox, ".config", "git-remote-signer", "signing.pub")
	got, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("read installed key: %v", err)
	}
	if strings.TrimSpace(string(got)) != s.pubLine {
		t.Errorf("installed key = %q, want %q", got, s.pubLine)
	}
}

// --- release server ----------------------------------------------------------

// releaseServer serves a release artifact and checksums.txt for the running
// platform, standing in for the GitHub release download until it is published.
type releaseServer struct {
	url      string
	artifact string
}

// newReleaseServer packs clientBin into a release tarball and serves it. When
// checksumOverride is non-empty it is used instead of the real digest, so a
// corrupted checksums.txt can be simulated.
func newReleaseServer(t *testing.T, clientBin, checksumOverride string) *releaseServer {
	t.Helper()
	const version = "v0.1.0"
	artifact := fmt.Sprintf("git-remote-sign_0.1.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	data := tarGz(t, "git-remote-sign", clientBin)
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if checksumOverride != "" {
		digest = checksumOverride
	}
	checksums := fmt.Sprintf("%s  %s\n", digest, artifact)

	mux := http.NewServeMux()
	mux.HandleFunc("/"+version+"/"+artifact, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(data)
	})
	mux.HandleFunc("/"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, checksums)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &releaseServer{url: ts.URL, artifact: artifact}
}

// tarGz returns a gzip-compressed tar holding srcPath as name with mode 0755.
func tarGz(t *testing.T, name, srcPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read %s: %v", srcPath, err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
