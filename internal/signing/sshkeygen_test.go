package signing_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSSHKeygenSignerRejectsOversizedPayload(t *testing.T) {
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{
		KeyPath:         "/nonexistent/key",
		MaxPayloadBytes: 16,
	})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}

	if _, err := signer.Sign(context.Background(), make([]byte, 17)); !errors.Is(err, signing.ErrPayloadTooLarge) {
		t.Fatalf("Sign(oversized) error = %v, want ErrPayloadTooLarge", err)
	}
	// A payload exactly at the limit is accepted (delegated to ssh-keygen, so
	// it fails only because the key is missing, never with ErrPayloadTooLarge).
	if _, err := signer.Sign(context.Background(), make([]byte, 16)); errors.Is(err, signing.ErrPayloadTooLarge) {
		t.Fatalf("Sign(at limit) error = %v, want no ErrPayloadTooLarge", err)
	}
}

func TestNewSSHKeygenSignerRequiresKeyPath(t *testing.T) {
	if _, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{}); err == nil {
		t.Fatal("NewSSHKeygenSigner(empty KeyPath) = nil error, want error")
	}
}

func TestSSHKeygenSignerHonoursTimeout(t *testing.T) {
	keyPath := newTestKey(t)
	base := t.TempDir()

	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{
		KeyPath: keyPath,
		Timeout: time.Nanosecond,
		TempDir: base,
	})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}

	if _, err := signer.Sign(context.Background(), []byte("payload")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sign(timed out) error = %v, want context.DeadlineExceeded", err)
	}
	assertEmptyDir(t, base)
}

func TestSSHKeygenSignerCleansUpTempFiles(t *testing.T) {
	keyPath := newTestKey(t)

	t.Run("success", func(t *testing.T) {
		base := t.TempDir()
		signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath, TempDir: base})
		if err != nil {
			t.Fatalf("NewSSHKeygenSigner: %v", err)
		}
		if _, err := signer.Sign(context.Background(), []byte("payload")); err != nil {
			t.Fatalf("Sign: %v", err)
		}
		assertEmptyDir(t, base)
	})

	t.Run("ssh-keygen failure", func(t *testing.T) {
		base := t.TempDir()
		signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{
			KeyPath: filepath.Join(t.TempDir(), "missing-key"),
			TempDir: base,
		})
		if err != nil {
			t.Fatalf("NewSSHKeygenSigner: %v", err)
		}
		if _, err := signer.Sign(context.Background(), []byte("payload")); err == nil {
			t.Fatal("Sign with missing key = nil error, want error")
		}
		assertEmptyDir(t, base)
	})
}

func TestSSHKeygenSignerConcurrentSigns(t *testing.T) {
	keyPath := newTestKey(t)
	base := t.TempDir()
	signer, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: keyPath, TempDir: base})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}

	const n = 8
	payloads := make([][]byte, n)
	sigs := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		payloads[i] = []byte(fmt.Sprintf("tree %d\n\npayload %d\n", i, i))
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sigs[i], errs[i] = signer.Sign(context.Background(), payloads[i])
		}(i)
	}
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent Sign[%d]: %v", i, errs[i])
		}
		verifySignature(t, sigs[i], payloads[i])
	}
	assertEmptyDir(t, base)
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("temp dir %s not cleaned up, contains %v", dir, names)
	}
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func TestPublicKeyDerivesFromPrivateKey(t *testing.T) {
	keyPath := newTestKey(t)

	got, err := signing.PublicKey(context.Background(), keyPath)
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}

	// Independent source of truth: the .pub file ssh-keygen wrote when it
	// generated the keypair.
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("read generated public key: %v", err)
	}
	fields := strings.Fields(string(pub))
	if len(fields) < 2 {
		t.Fatalf("unexpected generated public key: %q", pub)
	}
	gotFields := strings.Fields(got)
	if len(gotFields) < 2 {
		t.Fatalf("unexpected derived public key: %q", got)
	}
	if gotFields[0] != fields[0] || gotFields[1] != fields[1] {
		t.Fatalf("PublicKey = %q, want key type/blob %q", got, fields[0]+" "+fields[1])
	}
}

func TestPublicKeyMissingKeyErrors(t *testing.T) {
	if _, err := signing.PublicKey(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("PublicKey(missing key) = nil error, want error")
	}
	if _, err := signing.PublicKey(context.Background(), ""); err == nil {
		t.Fatal("PublicKey(empty path) = nil error, want error")
	}
}
