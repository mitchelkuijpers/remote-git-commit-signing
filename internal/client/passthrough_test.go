package client_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/client"
)

// sshKeygen returns the absolute path to the system ssh-keygen or skips.
func sshKeygen(t *testing.T) string {
	t.Helper()
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available:", err)
	}
	return keygen
}

// signPayload produces a real "git"-namespace SSHSIG over payload with keyPath.
func signPayload(t *testing.T, keyPath string, payload []byte) []byte {
	t.Helper()
	keygen := sshKeygen(t)
	dir := t.TempDir()
	buffer := filepath.Join(dir, "buffer")
	if err := os.WriteFile(buffer, payload, 0o600); err != nil {
		t.Fatalf("write buffer: %v", err)
	}
	cmd := exec.Command(keygen, "-Y", "sign", "-n", "git", "-f", keyPath, buffer)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y sign: %v: %s", err, out)
	}
	sig, err := os.ReadFile(buffer + ".sig")
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	return sig
}

// writeAllowedSigners writes an allowed-signers file mapping principal to the
// public key derived from keyPath and returns its path.
func writeAllowedSigners(t *testing.T, principal, keyPath string) string {
	t.Helper()
	pubLine := publicKeyLine(t, keyPath)
	fields := strings.Fields(pubLine)
	path := filepath.Join(t.TempDir(), "allowed_signers")
	content := principal + " " + fields[0] + " " + fields[1] + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write allowed signers: %v", err)
	}
	return path
}

// indirectExit runs ssh-keygen directly with args and returns its exit code.
func indirectExit(t *testing.T, args []string, stdin []byte) int {
	t.Helper()
	keygen := sshKeygen(t)
	cmd := exec.Command(keygen, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = new(bytes.Buffer)
	cmd.Stderr = new(bytes.Buffer)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("run ssh-keygen %v: %v", args, err)
	return -1
}

// TestVerifyPassthroughPreservesArgvAndExitCode proves the client execs
// ssh-keygen with the original argument slice verbatim (no -f rewriting) and
// propagates its exit code exactly.
func TestVerifyPassthroughPreservesArgvAndExitCode(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	stdinCopy := filepath.Join(dir, "stdin")
	script := "#!/bin/sh\n: > \"$RECORD\"\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$RECORD\"; done\n/bin/cat > \"$STDIN_COPY\"\nexit 7\n"
	writeFile(t, filepath.Join(dir, "ssh-keygen"), script)
	if err := os.Chmod(filepath.Join(dir, "ssh-keygen"), 0o755); err != nil {
		t.Fatalf("chmod wrapper: %v", err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("RECORD", record)
	t.Setenv("STDIN_COPY", stdinCopy)

	argv := []string{"git-remote-sign", "-Y", "verify", "-n", "git",
		"-f", "/some/allowed_signers", "-I", "dev@example.com",
		"-s", "/some/signature.sig", "-Overify-time=1700000000"}
	stdin := []byte("commit payload on stdin\n")

	var stdout, stderr bytes.Buffer
	code := client.Run(argv, env(nil), bytes.NewReader(stdin), &stdout, &stderr)

	if code != 7 {
		t.Fatalf("exit = %d, want ssh-keygen's 7; stderr=%s", code, stderr.String())
	}
	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	want := strings.Join(argv[1:], "\n") + "\n"
	if string(got) != want {
		t.Fatalf("ssh-keygen argv = %q, want %q (verbatim, program name stripped)", got, want)
	}
	stdinGot, err := os.ReadFile(stdinCopy)
	if err != nil {
		t.Fatalf("read recorded stdin: %v", err)
	}
	if !bytes.Equal(stdinGot, stdin) {
		t.Fatalf("ssh-keygen stdin = %q, want %q", stdinGot, stdin)
	}
}

// TestVerifyPassthroughRealSSHKeygen drives the real ssh-keygen through the
// client and checks the exit code matches ssh-keygen run directly, for valid,
// unmatched-principal and corrupt signatures.
func TestVerifyPassthroughRealSSHKeygen(t *testing.T) {
	keyPath := newTestKey(t)
	payload := []byte("tree 0000\nauthor A <a@example.com> 1 +0000\ncommitter C <c@example.com> 1 +0000\n\nsubject\n")
	sig := signPayload(t, keyPath, payload)
	allowed := writeAllowedSigners(t, "dev@example.com", keyPath)
	sigPath := filepath.Join(t.TempDir(), "commit.sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatalf("write sig: %v", err)
	}

	cases := []struct {
		name  string
		argv  []string
		stdin []byte
	}{
		{
			name:  "verify matching principal",
			argv:  []string{"git-remote-sign", "-Y", "verify", "-n", "git", "-f", allowed, "-I", "dev@example.com", "-s", sigPath},
			stdin: payload,
		},
		{
			name:  "verify unmatched principal",
			argv:  []string{"git-remote-sign", "-Y", "verify", "-n", "git", "-f", allowed, "-I", "other@example.com", "-s", sigPath},
			stdin: payload,
		},
		{
			name:  "verify tampered payload",
			argv:  []string{"git-remote-sign", "-Y", "verify", "-n", "git", "-f", allowed, "-I", "dev@example.com", "-s", sigPath},
			stdin: append([]byte("x"), payload...),
		},
		{
			name:  "find-principals",
			argv:  []string{"git-remote-sign", "-Y", "find-principals", "-f", allowed, "-s", sigPath},
			stdin: payload,
		},
		{
			name:  "check-novalidate",
			argv:  []string{"git-remote-sign", "-Y", "check-novalidate", "-n", "git", "-s", sigPath},
			stdin: payload,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// No client configuration is provided: passthrough must not need it.
			code := client.Run(tc.argv, env(nil), bytes.NewReader(tc.stdin), &stdout, &stderr)

			want := indirectExit(t, tc.argv[1:], tc.stdin)
			if code != want {
				t.Fatalf("client exit = %d, want ssh-keygen's %d; stderr=%s", code, want, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Fatalf("client captured no ssh-keygen stdout")
			}
		})
	}
}

// TestFindPrincipalsPrintsPrincipal confirms stdout is wired through by
// checking the matched principal appears in the client's captured output.
func TestFindPrincipalsPrintsPrincipal(t *testing.T) {
	keyPath := newTestKey(t)
	payload := []byte("payload\n")
	sig := signPayload(t, keyPath, payload)
	allowed := writeAllowedSigners(t, "dev@example.com", keyPath)
	sigPath := filepath.Join(t.TempDir(), "sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatalf("write sig: %v", err)
	}

	var stdout, stderr bytes.Buffer
	argv := []string{"git-remote-sign", "-Y", "find-principals", "-f", allowed, "-s", sigPath}
	if code := client.Run(argv, env(nil), bytes.NewReader(payload), &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "dev@example.com") {
		t.Fatalf("stdout = %q, want the matched principal", stdout.String())
	}
}
