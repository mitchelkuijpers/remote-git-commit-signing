// Package signing produces OpenSSH SSHSIG signatures for Git commit payloads.
//
// The concrete ssh-keygen backend shells out to the system ssh-keygen; the
// Signer interface hides that choice so a different backend (for example an
// HSM or a hardware token) can replace it without touching callers.
package signing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// namespaceGit is the only SSHSIG namespace Git uses for commit signing.
const namespaceGit = "git"

// Defaults applied when the corresponding SSHKeygenConfig field is zero.
const (
	// DefaultMaxPayloadBytes caps the payload size accepted by a signer.
	DefaultMaxPayloadBytes = 1 << 20 // 1 MiB
	// DefaultTimeout bounds a single ssh-keygen invocation.
	DefaultTimeout = 5 * time.Second
)

// ErrPayloadTooLarge is returned when a payload exceeds the configured limit.
var ErrPayloadTooLarge = errors.New("signing: payload too large")

// Signer signs a payload and returns the raw OpenSSH SSHSIG PEM block.
//
// Implementations must be safe for concurrent use.
type Signer interface {
	Sign(ctx context.Context, payload []byte) ([]byte, error)
}

// SSHKeygenConfig configures an SSHKeygenSigner.
type SSHKeygenConfig struct {
	// KeyPath is the path to the private signing key. Required.
	KeyPath string
	// MaxPayloadBytes is the largest payload accepted. Defaults to
	// DefaultMaxPayloadBytes when zero.
	MaxPayloadBytes int64
	// Timeout bounds a single signing invocation. Defaults to DefaultTimeout
	// when zero.
	Timeout time.Duration
	// TempDir is the parent directory for per-request working directories.
	// Defaults to the system temporary directory when empty.
	TempDir string
}

// SSHKeygenSigner signs payloads by invoking the system ssh-keygen. It holds no
// mutable state, so a single value may be used concurrently.
type SSHKeygenSigner struct {
	keyPath  string
	keygen   string
	maxBytes int64
	timeout  time.Duration
	baseTmp  string
}

var _ Signer = (*SSHKeygenSigner)(nil)

// NewSSHKeygenSigner validates cfg and returns a ready signer.
func NewSSHKeygenSigner(cfg SSHKeygenConfig) (*SSHKeygenSigner, error) {
	if cfg.KeyPath == "" {
		return nil, errors.New("signing: SSHKeygenConfig.KeyPath is required")
	}

	s := &SSHKeygenSigner{
		keyPath:  cfg.KeyPath,
		keygen:   "ssh-keygen",
		maxBytes: cfg.MaxPayloadBytes,
		timeout:  cfg.Timeout,
		baseTmp:  cfg.TempDir,
	}
	if s.maxBytes <= 0 {
		s.maxBytes = DefaultMaxPayloadBytes
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	return s, nil
}

// PublicKey returns the OpenSSH authorized_keys line for the private key at
// keyPath (for example "ssh-ed25519 AAAA...") by running ssh-keygen -y. It
// never writes or logs the private key material.
func PublicKey(ctx context.Context, keyPath string) (string, error) {
	if keyPath == "" {
		return "", errors.New("signing: key path is required")
	}

	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-y", "-f", keyPath)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("signing: ssh-keygen -y: %w", ctxErr)
		}
		return "", fmt.Errorf("signing: ssh-keygen -y: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Sign implements Signer. The payload is written to a unique, access-restricted
// working directory, ssh-keygen signs it without a shell, and the working
// directory is removed on every path.
func (s *SSHKeygenSigner) Sign(ctx context.Context, payload []byte) ([]byte, error) {
	if int64(len(payload)) > s.maxBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds limit of %d bytes",
			ErrPayloadTooLarge, len(payload), s.maxBytes)
	}

	dir, err := os.MkdirTemp(s.baseTmp, "git-signer-")
	if err != nil {
		return nil, fmt.Errorf("signing: create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	payloadPath := filepath.Join(dir, "payload")
	if err := os.WriteFile(payloadPath, payload, 0o600); err != nil {
		return nil, fmt.Errorf("signing: write payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, s.keygen, "-Y", "sign", "-n", namespaceGit, "-f", s.keyPath, payloadPath)
	cmd.Dir = dir
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("signing: ssh-keygen: %w", ctxErr)
		}
		return nil, fmt.Errorf("signing: ssh-keygen: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}

	sig, err := os.ReadFile(payloadPath + ".sig")
	if err != nil {
		return nil, fmt.Errorf("signing: read signature: %w", err)
	}
	return sig, nil
}
