package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

// Run executes the git-remote-sign program and returns its exit code.
//
// argv is os.Args form; getenv is normally os.Getenv; diagnostic output is
// written to stderr. On any failure Run removes a partial <bufferfile>.sig and
// returns non-zero so Git aborts the commit.
func Run(argv []string, getenv func(string) string, stderr io.Writer) int {
	buffer, err := run(argv, getenv)
	if err != nil {
		if buffer != "" {
			// Never leave a partial/unverified signature behind.
			os.Remove(buffer + ".sig")
		}
		fmt.Fprintf(stderr, "git-remote-sign: %v\n", err)
		return 1
	}
	return 0
}

// run performs one invocation. It returns the signing buffer path (when known)
// so that Run can clean up a partial .sig on failure.
func run(argv []string, getenv func(string) string) (bufferFile string, err error) {
	inv, err := parseInvocation(argv)
	if err != nil {
		return "", err
	}
	if inv.op != "sign" {
		// Verification operations are delegated to real ssh-keygen in a later
		// slice; fail loudly rather than silently mis-handling them.
		return "", fmt.Errorf("operation %q is not implemented yet", inv.op)
	}
	bufferFile = inv.bufferFile

	cfg, err := loadConfig(getenv)
	if err != nil {
		return bufferFile, err
	}

	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return bufferFile, fmt.Errorf("ssh-keygen not found in PATH: %w", err)
	}

	// The key Git asks us to sign with must be exactly the pinned trusted key.
	requested, err := readPublicKeyFile(inv.keyFile)
	if err != nil {
		return bufferFile, err
	}
	if !requested.equal(cfg.pinned) {
		return bufferFile, fmt.Errorf("signing key %s does not match the pinned %s key", inv.keyFile, envPublicKey)
	}

	payload, err := os.ReadFile(bufferFile)
	if err != nil {
		return bufferFile, fmt.Errorf("read signing buffer %s: %w", bufferFile, err)
	}

	sig, err := fetchSignature(cfg, payload)
	if err != nil {
		return bufferFile, err
	}

	if err := verifySignature(context.Background(), keygen, cfg.pinned, payload, sig); err != nil {
		return bufferFile, err
	}

	if err := writeSignatureAtomic(bufferFile+".sig", sig); err != nil {
		return bufferFile, err
	}
	return bufferFile, nil
}

// fetchSignature POSTs the exact payload bytes to the signer and returns the
// raw SSHSIG response, bounded by the configured response size cap.
func fetchSignature(cfg config, payload []byte) ([]byte, error) {
	endpoint := cfg.signURL + "/v1/sign"

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build signing request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(payload))

	client := &http.Client{Timeout: cfg.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request signature from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read signature response: %w", err)
	}
	if int64(len(body)) > cfg.maxResponseBytes {
		return nil, fmt.Errorf("signature response exceeds %d bytes", cfg.maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("signer returned %s: %s", resp.Status, snippet(body))
	}
	if len(body) == 0 {
		return nil, errors.New("signer returned an empty signature")
	}
	return body, nil
}

// writeSignatureAtomic writes sig to path via a temp file in the same directory
// followed by a rename, so a partially written .sig is never observable.
func writeSignatureAtomic(path string, sig []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".git-remote-sign-*.sig")
	if err != nil {
		return fmt.Errorf("create temporary signature: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()

	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("set signature permissions: %w", err)
	}
	if _, err := f.Write(sig); err != nil {
		f.Close()
		return fmt.Errorf("write signature: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync signature: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close signature: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install signature %s: %w", path, err)
	}
	tmp = ""
	return nil
}

// snippet limits a response body to a short, log-safe excerpt.
func snippet(b []byte) string {
	const max = 200
	b = bytes.TrimSpace(b)
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
