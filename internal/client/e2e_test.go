package client_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/setting"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// End-to-end (spec seam 3): the REAL git binary drives the built
// git-remote-sign program, which talks to a real listener running the real
// signer server. Nothing is faked except the exe.dev platform plumbing, whose
// verified source-VM identity the harness injects on ingress exactly
// as the transparent production proxy presents it.

const (
	e2eVMIdentity = "e2e-test-vm"
	e2eName       = "Test Agent"
	e2eEmail      = "agent@example.com"
)

// moduleRoot returns the module root (the directory holding go.mod) so the test
// can build the command binaries.
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

// buildBinaries compiles the two command binaries into a temp dir and returns
// the path of git-remote-sign.
func buildBinaries(t *testing.T) (remoteSign, signerServer string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available:", err)
	}
	binDir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", binDir,
		"./cmd/git-remote-sign", "./cmd/git-signer-server")
	cmd.Dir = moduleRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	return filepath.Join(binDir, "git-remote-sign"), filepath.Join(binDir, "git-signer-server")
}

// e2eServer is a running signer server plus the configuration the git repos
// need to reach it.
type e2eServer struct {
	url        string
	pubKey     string // authorized_keys line
	keyPath    string
	pubKeyPath string
}

// startServer starts the real internal/server.Server on a real HTTP listener,
// backed by a throwaway ED25519 key.
func startServer(t *testing.T) *e2eServer {
	t.Helper()
	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)

	pubKeyPath := filepath.Join(t.TempDir(), "signing.pub")
	writeFile(t, pubKeyPath, pubLine+"\n")

	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}

	cfg := server.HandlerConfig{
		Committer:  server.Committer{Name: e2eName, Email: e2eEmail},
		Allowlist:  server.Allowlist{e2eVMIdentity},
		RatePerMin: 6000,
		RateBurst:  1000,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(signer, pubLine, cfg, logger)

	// Simulate the exe.dev peer integration: the platform's authenticated proxy
	// stamps the platform-verified source-VM identity on ingress via the
	// exported server helper and no client can forge it. Authz then runs
	// unchanged.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.StampSourceVM(r.Header, e2eVMIdentity)
		srv.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return &e2eServer{url: ts.URL, pubKey: pubLine, keyPath: keyPath, pubKeyPath: pubKeyPath}
}

// clientEnv is the environment every git invocation inherits so that Git's
// signing program can reach the server and pin the key.
func (s *e2eServer) clientEnv(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{
		setting.SignerURL:       s.url,
		setting.SignerPublicKey: s.pubKeyPath,
		"HOME":                  t.TempDir(),
		"GIT_CONFIG_GLOBAL":     os.DevNull,
		"GIT_CONFIG_NOSYSTEM":   "1",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// result is a finished subprocess.
type result struct {
	exit           int
	stdout, stderr string
}

func (r result) String() string {
	return fmt.Sprintf("exit=%d stdout=%q stderr=%q", r.exit, r.stdout, r.stderr)
}

// runEnv runs name with args in dir, merging env over the process environment.
func runEnv(t *testing.T, dir string, env map[string]string, name string, args ...string) result {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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

// allowedSigners writes an allowed-signers file mapping principal to keyPath's
// public key and returns its path.
func allowedSignersFile(t *testing.T, principal, keyPath string) string {
	t.Helper()
	pubLine := publicKeyLine(t, keyPath)
	fields := strings.Fields(pubLine)
	path := filepath.Join(t.TempDir(), "allowed_signers")
	writeFile(t, path, principal+" "+fields[0]+" "+fields[1]+"\n")
	return path
}

// initRepo creates a repository configured exactly like the documented client
// contract: SSH signing format, our program, auto-signing, the pinned public
// key, and an allowed-signers file.
func initRepo(t *testing.T, remoteSign string, s *e2eServer, allowedSigners string) string {
	t.Helper()
	dir := t.TempDir()
	env := s.clientEnv(t, nil)
	mustGit(t, dir, env, "init", "-q", "-b", "main")
	for _, kv := range [][2]string{
		{"user.name", e2eName},
		{"user.email", e2eEmail},
		{"gpg.format", "ssh"},
		{"commit.gpgsign", "true"},
		{"gpg.ssh.program", remoteSign},
		{"user.signingkey", s.pubKeyPath},
		{"gpg.ssh.allowedSignersFile", allowedSigners},
	} {
		mustGit(t, dir, env, "config", kv[0], kv[1])
	}
	return dir
}

func mustGit(t *testing.T, dir string, env map[string]string, args ...string) result {
	t.Helper()
	r := runEnv(t, dir, env, "git", args...)
	if r.exit != 0 {
		t.Fatalf("git %s: %s", strings.Join(args, " "), r)
	}
	return r
}

// commitFile stages content and commits it, returning the git result.
func commitFile(t *testing.T, dir string, env map[string]string, content string) result {
	t.Helper()
	writeFile(t, filepath.Join(dir, "file.txt"), content)
	mustGit(t, dir, env, "add", "file.txt")
	return runEnv(t, dir, env, "git", "commit", "-m", "subject line\n\nbody\n")
}

// corruptSignature replaces a commit's embedded SSH signature with a garbage
// token, dropping the signature's continuation lines and leaving the rest of
// the object intact. This is the "garbage signature embedded" case the spike
// documented: Git accepts it at commit time and verification fails fatally.
func corruptSignature(commit string) string {
	lines := strings.Split(commit, "\n")
	out := make([]string, 0, len(lines))
	skip := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "gpgsig "):
			out = append(out, "gpgsig not-a-signature")
			skip = true
		case skip && strings.HasPrefix(line, " "):
			// Drop the corrupted header's continuation lines.
		default:
			skip = false
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func TestE2ECommitAndVerify(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available:", err)
	}
	sshKeygen(t)
	remoteSign, _ := buildBinaries(t)
	s := startServer(t)
	allowed := allowedSignersFile(t, e2eEmail, s.keyPath)
	repo := initRepo(t, remoteSign, s, allowed)
	env := s.clientEnv(t, nil)

	c := commitFile(t, repo, env, "hello\n")
	if c.exit != 0 {
		t.Fatalf("git commit failed: %s", c)
	}

	raw := mustGit(t, repo, env, "cat-file", "commit", "HEAD")
	if !strings.Contains(raw.stdout, "-----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("commit does not embed an SSHSIG header:\n%s", raw.stdout)
	}

	v := runEnv(t, repo, env, "git", "verify-commit", "HEAD")
	if v.exit != 0 {
		t.Fatalf("git verify-commit = %d, want 0: %s", v.exit, v)
	}
}

func TestE2EVerifyUnknownSignerExitsOne(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available:", err)
	}
	sshKeygen(t)
	remoteSign, _ := buildBinaries(t)
	s := startServer(t)
	allowed := allowedSignersFile(t, e2eEmail, s.keyPath)
	repo := initRepo(t, remoteSign, s, allowed)
	env := s.clientEnv(t, nil)

	if c := commitFile(t, repo, env, "hello\n"); c.exit != 0 {
		t.Fatalf("git commit failed: %s", c)
	}

	// Point verification at an allowed-signers file that does not cover the
	// signing key: no principal matches.
	otherKey := newTestKey(t)
	otherAllowed := allowedSignersFile(t, "someone-else@example.com", otherKey)
	mustGit(t, repo, env, "config", "gpg.ssh.allowedSignersFile", otherAllowed)

	v := runEnv(t, repo, env, "git", "verify-commit", "HEAD")
	if v.exit != 1 {
		t.Fatalf("git verify-commit with unknown signer = %d, want 1: %s", v.exit, v)
	}
}

func TestE2EVerifyCorruptSignatureExits128(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available:", err)
	}
	sshKeygen(t)
	remoteSign, _ := buildBinaries(t)
	s := startServer(t)
	allowed := allowedSignersFile(t, e2eEmail, s.keyPath)
	repo := initRepo(t, remoteSign, s, allowed)
	env := s.clientEnv(t, nil)

	if c := commitFile(t, repo, env, "hello\n"); c.exit != 0 {
		t.Fatalf("git commit failed: %s", c)
	}

	raw := mustGit(t, repo, env, "cat-file", "commit", "HEAD").stdout
	// Tamper the signed commit: replace its embedded signature with garbage
	// while leaving the object's other headers and message intact.
	tampered := corruptSignature(raw)

	// hash-object needs the tampered content on stdin; run it manually.
	cmd := exec.Command("git", "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), envPairs(env)...)
	cmd.Stdin = strings.NewReader(tampered)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git hash-object: %v: %s", err, errb.String())
	}
	newSHA := strings.TrimSpace(out.String())
	mustGit(t, repo, env, "update-ref", "refs/heads/main", newSHA)

	v := runEnv(t, repo, env, "git", "verify-commit", "HEAD")
	if v.exit != 128 {
		t.Fatalf("git verify-commit on corrupt signature = %d, want 128: %s", v.exit, v)
	}
}

func TestE2EConcurrentCommitsAllVerify(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available:", err)
	}
	sshKeygen(t)
	remoteSign, _ := buildBinaries(t)
	s := startServer(t)
	allowed := allowedSignersFile(t, e2eEmail, s.keyPath)

	const n = 5
	repos := make([]string, n)
	for i := range repos {
		repos[i] = initRepo(t, remoteSign, s, allowed)
	}

	type outcome struct {
		idx            int
		commit, verify result
	}
	results := make([]outcome, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := s.clientEnv(t, nil)
			c := commitFile(t, repos[i], env, fmt.Sprintf("content %d\n", i))
			v := runEnv(t, repos[i], env, "git", "verify-commit", "HEAD")
			results[i] = outcome{idx: i, commit: c, verify: v}
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		if r.commit.exit != 0 {
			t.Errorf("repo %d: git commit failed: %s", r.idx, r.commit)
		}
		if r.verify.exit != 0 {
			t.Errorf("repo %d: git verify-commit = %d, want 0: %s", r.idx, r.verify.exit, r.verify)
		}
	}
}

func TestE2ESignerFailureLeavesNoCommitAndNoSig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available:", err)
	}
	sshKeygen(t)

	// A stub signer that always fails, so the client cannot obtain a signature.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		http.Error(w, "signing failed", http.StatusInternalServerError)
	}))
	t.Cleanup(stub.Close)

	// Build only the client; the server is irrelevant here.
	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", binDir, "./cmd/git-remote-sign")
	build.Dir = moduleRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	remoteSign := filepath.Join(binDir, "git-remote-sign")

	keyPath := newTestKey(t)
	pubLine := publicKeyLine(t, keyPath)
	pubKeyPath := filepath.Join(t.TempDir(), "signing.pub")
	writeFile(t, pubKeyPath, pubLine+"\n")
	allowed := allowedSignersFile(t, e2eEmail, keyPath)

	tmpDir := t.TempDir()
	env := map[string]string{
		setting.SignerURL:       stub.URL,
		setting.SignerPublicKey: pubKeyPath,
		"HOME":                  t.TempDir(),
		"GIT_CONFIG_GLOBAL":     os.DevNull,
		"GIT_CONFIG_NOSYSTEM":   "1",
		"TMPDIR":                tmpDir,
	}

	repo := t.TempDir()
	mustGit(t, repo, env, "init", "-q", "-b", "main")
	for _, kv := range [][2]string{
		{"user.name", e2eName},
		{"user.email", e2eEmail},
		{"gpg.format", "ssh"},
		{"commit.gpgsign", "true"},
		{"gpg.ssh.program", remoteSign},
		{"user.signingkey", pubKeyPath},
		{"gpg.ssh.allowedSignersFile", allowed},
	} {
		mustGit(t, repo, env, "config", kv[0], kv[1])
	}

	c := commitFile(t, repo, env, "hello\n")
	if c.exit != 128 {
		t.Fatalf("git commit with failing signer = %d, want 128: %s", c.exit, c)
	}

	// No commit was created.
	if r := runEnv(t, repo, env, "git", "rev-parse", "--verify", "HEAD"); r.exit == 0 {
		t.Fatalf("HEAD exists after a failed signed commit: %s", r)
	}

	// No partial signature was left behind next to Git's signing buffer.
	sigs, err := filepath.Glob(filepath.Join(tmpDir, "*.sig"))
	if err != nil {
		t.Fatalf("glob sig files: %v", err)
	}
	if len(sigs) != 0 {
		t.Fatalf("partial signature files left behind: %v", sigs)
	}
	repoSigs, err := filepath.Glob(filepath.Join(repo, "*.sig"))
	if err != nil {
		t.Fatalf("glob repo sig files: %v", err)
	}
	if len(repoSigs) != 0 {
		t.Fatalf("partial signature files left in repo: %v", repoSigs)
	}
}

// envPairs renders an environment map as KEY=VALUE strings suitable for
// exec.Cmd.Env.
func envPairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for k, v := range env {
		pairs = append(pairs, k+"="+v)
	}
	return pairs
}
