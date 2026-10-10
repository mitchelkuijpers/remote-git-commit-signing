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
	"strings"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/setting"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/wire"
)

// Run executes the git-remote-sign program and returns its exit code.
//
// argv is os.Args form; getenv is normally os.Getenv; stdin/stdout/stderr are
// the process's standard streams. Signing failures remove a partial
// <bufferfile>.sig and return non-zero so Git aborts the commit; verify
// operations return ssh-keygen's own exit code unchanged.
func Run(argv []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	inv, err := parseInvocation(argv)
	if err != nil {
		fmt.Fprintf(stderr, "git-remote-sign: %v\n", err)
		return 1
	}

	// Verification (and its fallbacks) belongs to the real ssh-keygen: Git
	// drives its own two-step protocol through this program and, in verify
	// mode, passes the allowed-signers file as -f. Delegate verbatim.
	if isPassthroughOperation(inv.op) {
		return runPassthrough(context.Background(), argv, stdin, stdout, stderr)
	}

	if inv.op != "sign" {
		// Unknown operations fail loudly rather than being handled wrongly.
		fmt.Fprintf(stderr, "git-remote-sign: operation %q is not implemented yet\n", inv.op)
		return 1
	}

	if err := runSign(inv, getenv); err != nil {
		// Never leave a partial/unverified signature behind.
		os.Remove(inv.bufferFile + ".sig")
		fmt.Fprintf(stderr, "git-remote-sign: %v\n", err)
		return 1
	}
	return 0
}

// runSign performs one signing invocation for a fully validated invocation.
func runSign(inv invocation, getenv func(string) string) error {
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}

	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		return fmt.Errorf("ssh-keygen not found in PATH: %w", err)
	}

	// The key Git asks us to sign with must be exactly the pinned trusted key.
	requested, err := readPublicKeyFile(inv.keyFile)
	if err != nil {
		return err
	}
	if !requested.equal(cfg.pinned) {
		return fmt.Errorf("signing key %s does not match the pinned %s key", inv.keyFile, setting.SignerPublicKey)
	}

	payload, err := os.ReadFile(inv.bufferFile)
	if err != nil {
		return fmt.Errorf("read signing buffer %s: %w", inv.bufferFile, err)
	}

	sig, err := fetchSignature(cfg, payload)
	if err != nil {
		return err
	}

	if err := verifySignature(context.Background(), keygen, cfg.pinned, payload, sig); err != nil {
		return err
	}

	if err := writeSignatureAtomic(inv.bufferFile+".sig", sig); err != nil {
		return err
	}
	return nil
}

// fetchSignature POSTs the exact payload bytes to the signer and returns the
// raw SSHSIG response, bounded by the configured response size cap.
func fetchSignature(cfg config, payload []byte) ([]byte, error) {
	endpoint := cfg.signURL + wire.SignPath

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build signing request: %w", err)
	}
	req.Header.Set("Content-Type", wire.ContentTypeOctetStream)
	req.ContentLength = int64(len(payload))

	// Do not follow redirects: for 301/302 Go would silently downgrade the
	// POST to a GET and the caller would see an opaque 405. A redirect here
	// means a misconfigured signer URL (classically http:// on an edge that
	// redirects to https://), so surface it with a specific message instead.
	client := &http.Client{
		Timeout: cfg.timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request signature from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("signer redirected the signing request (HTTP %d to %q): update %s to the redirect target — note the exe.dev peer integration uses https://, not http://", resp.StatusCode, resp.Header.Get("Location"), setting.SignerURL)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read signature response: %w", err)
	}
	if int64(len(body)) > cfg.maxResponseBytes {
		return nil, fmt.Errorf("signature response exceeds %d bytes", cfg.maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		// The response body is untrusted: the signer may be misconfigured,
		// compromised, or MITM'd over cleartext HTTP, and it could reflect the
		// commit payload or embed terminal escapes. Report the status plus a
		// sanitized summary, never the raw bytes.
		if summary := responseSummary(body); summary != "" {
			return nil, fmt.Errorf("signer returned %s: %s", resp.Status, summary)
		}
		return nil, fmt.Errorf("signer returned %s", resp.Status)
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

// responseSummary reduces an untrusted response body to a short, terminal-safe
// excerpt for an error message. Only the first line is kept, bytes outside
// printable ASCII are dropped (so control characters and terminal escapes
// cannot reach the user's terminal), and the result is capped. It returns ""
// when nothing printable remains.
func responseSummary(b []byte) string {
	const max = 200
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	printable := make([]byte, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			printable = append(printable, c)
		}
	}
	s := strings.TrimSpace(string(printable))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
