package client

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// verifyTimeout bounds a single local ssh-keygen verification invocation.
const verifyTimeout = 10 * time.Second

// allowedPrincipal is the principal the client pins the trusted key under in
// the temporary allowed_signers file used for local verification. Git's own
// verification uses user-supplied principals; here it only needs to be a fixed
// non-empty name that matches -I.
const allowedPrincipal = "git-remote-sign"

// verifySignature cryptographically verifies sig over payload in the "git"
// namespace against the pinned key, using the system ssh-keygen.
//
// It uses "ssh-keygen -Y verify" with an allowed_signers file containing only
// the pinned key. That single invocation checks all three properties the client
// must guarantee before writing .sig:
//
//  1. the signature is well-formed and made over exactly these payload bytes,
//  2. it is bound to the "git" namespace, and
//  3. it was produced by the pinned public key (not merely some key).
//
// "check-novalidate" is deliberately not used: OpenSSH 9.6 accepts and ignores
// its -f flag, so it verifies the signature against the key embedded in the
// signature itself. A wrong-key signature would pass, defeating the pin.
func verifySignature(ctx context.Context, keygen string, pinned publicKey, payload, sig []byte) error {
	dir, err := os.MkdirTemp("", "git-remote-sign-verify-")
	if err != nil {
		return fmt.Errorf("create verification temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	allowedPath := filepath.Join(dir, "allowed_signers")
	allowed := fmt.Sprintf("%s %s %s\n", allowedPrincipal, pinned.typ, pinned.blob)
	if err := os.WriteFile(allowedPath, []byte(allowed), 0o600); err != nil {
		return fmt.Errorf("write allowed signers: %w", err)
	}

	sigPath := filepath.Join(dir, "signature.sig")
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		return fmt.Errorf("write signature for verification: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, keygen, "-Y", "verify",
		"-n", "git",
		"-f", allowedPath,
		"-I", allowedPrincipal,
		"-s", sigPath)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("verify signature: %w", ctxErr)
		}
		return fmt.Errorf("signature failed local verification against pinned key: %w: %s",
			err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}
