package signing_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// requireSSHKeygen skips the test when the system ssh-keygen is unavailable.
func requireSSHKeygen(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not available:", err)
	}
	return path
}

// newTestKey generates a throwaway unencrypted ED25519 keypair inside the
// test's temporary directory and returns the private key path.
func newTestKey(t *testing.T) string {
	t.Helper()
	keygen := requireSSHKeygen(t)
	path := filepath.Join(t.TempDir(), "id_ed25519")
	cmd := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "test@example.com", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate test key: %v: %s", err, out)
	}
	return path
}

// verifySignature asserts that stock ssh-keygen accepts sig as a valid SSHSIG
// over payload in the git namespace.
func verifySignature(t *testing.T, sig, payload []byte) {
	t.Helper()
	keygen := requireSSHKeygen(t)
	dir := t.TempDir()
	sigPath := filepath.Join(dir, "payload.sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		t.Fatalf("write signature: %v", err)
	}
	cmd := exec.Command(keygen, "-Y", "check-novalidate", "-n", "git", "-s", sigPath)
	cmd.Stdin = bytes.NewReader(payload)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen check-novalidate rejected signature: %v: %s", err, out)
	}
}

func TestSSHKeygenSignerSignsVerifiableSignature(t *testing.T) {
	keyPath := newTestKey(t)

	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}

	payload := []byte("tree 0123456789abcdef\ncommitter Test <test@example.com> 1700000000 +0000\n\nsubject\n")

	sig, err := signer.Sign(context.Background(), payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !bytes.HasPrefix(sig, []byte("-----BEGIN SSH SIGNATURE-----")) {
		t.Fatalf("signature is not an SSHSIG PEM block: %q", firstLine(sig))
	}

	// The signature must be independent of our implementation: stock ssh-keygen
	// re-validates it against the payload.
	verifySignature(t, sig, payload)
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
