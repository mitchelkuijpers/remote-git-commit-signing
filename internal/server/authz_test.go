package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// do sends req and returns the response with its body read into a string.
func do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body of %s %s: %v", req.Method, req.URL, err)
	}
	return resp, string(body)
}

// postSignAs posts payload to the sign endpoint carrying vm as the verified
// source-VM identity. An empty vm sends no identity header at all.
func postSignAs(t *testing.T, baseURL, vm string, payload []byte) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/sign", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new sign request: %v", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if vm != "" {
		req.Header.Set("X-Exedev-Source-Vm", vm)
	}
	return do(t, req)
}

func TestSignWithoutIdentityIsUnauthorized(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{Allowlist: server.Allowlist{"test-vm"}})

	resp, body := postSignAs(t, ts.URL, "", []byte("payload"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /v1/sign without identity = %d, want 401: %s", resp.StatusCode, body)
	}
	if strings.HasPrefix(body, "-----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("POST /v1/sign without identity produced a signature: %q", body)
	}
}

func TestSignRejectsNonAllowlistedIdentity(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{Allowlist: server.Allowlist{"test-vm"}})

	resp, body := postSignAs(t, ts.URL, "intruder", []byte("payload"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /v1/sign from non-allowlisted VM = %d, want 403: %s", resp.StatusCode, body)
	}
	if strings.HasPrefix(body, "-----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("POST /v1/sign from non-allowlisted VM produced a signature: %q", body)
	}
}

func TestSignAllowlistSupportsExactNamesAndPatterns(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{
		Allowlist:      server.Allowlist{"exact-vm", "agent-*"},
		CommitterName:  testCommitterName,
		CommitterEmail: testCommitterEmail,
		RateBurst:      1000,
	})

	commit := validCommit(testCommitterName, testCommitterEmail, "message\n")

	tests := map[string]int{
		"exact-vm": http.StatusOK,
		"agent-1":  http.StatusOK,
		"agent-":   http.StatusOK,
		"agent":    http.StatusForbidden,
		"other":    http.StatusForbidden,
	}
	for vm, want := range tests {
		t.Run(vm, func(t *testing.T) {
			resp, body := postSignAs(t, ts.URL, vm, commit)
			if resp.StatusCode != want {
				t.Fatalf("POST /v1/sign as %q = %d, want %d: %s", vm, resp.StatusCode, want, body)
			}
		})
	}
}

func TestSignEmptyAllowlistRefusesEveryone(t *testing.T) {
	signer, publicKey := newSigner(t)
	// No allowlist configured at all: fail closed, nobody may sign.
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{})

	resp, body := postSignAs(t, ts.URL, "test-vm", []byte("payload"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /v1/sign with unset allowlist = %d, want 403: %s", resp.StatusCode, body)
	}
}

func TestSignRateLimitIsPerIdentity(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{
		Allowlist:      server.Allowlist{"vm-a", "vm-b"},
		CommitterName:  testCommitterName,
		CommitterEmail: testCommitterEmail,
		RatePerMin:     1,
		RateBurst:      2,
	})

	commit := validCommit(testCommitterName, testCommitterEmail, "message\n")

	for i, want := range []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests} {
		resp, body := postSignAs(t, ts.URL, "vm-a", commit)
		if resp.StatusCode != want {
			t.Fatalf("vm-a request %d = %d, want %d: %s", i+1, resp.StatusCode, want, body)
		}
	}

	// A different VM has its own bucket and is unaffected by vm-a's exhaustion.
	resp, body := postSignAs(t, ts.URL, "vm-b", commit)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vm-b request = %d, want 200 (independent bucket): %s", resp.StatusCode, body)
	}
}

func TestPublicEndpointsDoNotRequireIdentity(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, server.Config{Allowlist: server.Allowlist{"test-vm"}})

	for _, path := range []string{"/", "/healthz", "/readyz", "/v1/public-key"} {
		resp, body := get(t, ts.URL+path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s without identity = %d, want 200: %s", path, resp.StatusCode, body)
		}
	}
}

// logLines parses the JSON log lines in raw.
func logLines(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %v: %q", err, line)
		}
		out = append(out, entry)
	}
	return out
}

func TestAuditLogLinePerSigningDecision(t *testing.T) {
	signer, publicKey := newSigner(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := server.New(signer, publicKey, server.Config{
		Allowlist:      server.Allowlist{"agent-*"},
		CommitterName:  testCommitterName,
		CommitterEmail: testCommitterEmail,
		RateBurst:      1000,
	}, logger)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	payload := validCommit(testCommitterName, testCommitterEmail, "secret-subject-line\n")

	resp, _ := postSignAs(t, ts.URL, "agent-1", payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted sign = %d, want 200", resp.StatusCode)
	}
	if resp, _ := postSignAs(t, ts.URL, "", payload); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("identity-less sign = %d, want 401", resp.StatusCode)
	}
	if resp, _ := postSignAs(t, ts.URL, "nope", payload); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-allowlisted sign = %d, want 403", resp.StatusCode)
	}

	lines := logLines(t, buf.String())
	if len(lines) != 3 {
		t.Fatalf("audit lines = %d, want exactly one per signing decision (3): %v", len(lines), lines)
	}

	// The payload itself must never appear in the log, only its hash.
	if strings.Contains(buf.String(), "secret-subject-line") {
		t.Fatalf("audit log leaked payload bytes: %q", buf.String())
	}

	wantHash := sha256.Sum256(payload)
	for _, entry := range lines {
		if entry["event"] != "sign_request" {
			t.Fatalf("audit line event = %v, want sign_request: %v", entry["event"], entry)
		}
		if _, ok := entry["duration_ms"]; !ok {
			t.Fatalf("audit line missing duration_ms: %v", entry)
		}
		if _, ok := entry["status"]; !ok {
			t.Fatalf("audit line missing status: %v", entry)
		}
		if _, ok := entry["vm"]; !ok {
			t.Fatalf("audit line missing vm: %v", entry)
		}
	}

	if got := lines[0]["vm"]; got != "agent-1" {
		t.Fatalf("success audit vm = %v, want agent-1", got)
	}
	if got := lines[0]["status"]; got != float64(http.StatusOK) {
		t.Fatalf("success audit status = %v, want 200", got)
	}
	if got := lines[0]["payload_sha256"]; got != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("success audit payload_sha256 = %v, want %s", got, hex.EncodeToString(wantHash[:]))
	}
	if got := lines[0]["outcome"]; got != "signed" {
		t.Fatalf("success audit outcome = %v, want signed", got)
	}

	if got := lines[1]["vm"]; got != "" {
		t.Fatalf("identity-less audit vm = %v, want empty", got)
	}
	if got := lines[1]["status"]; got != float64(http.StatusUnauthorized) {
		t.Fatalf("identity-less audit status = %v, want 401", got)
	}
	if got := lines[1]["outcome"]; got != "rejected" {
		t.Fatalf("identity-less audit outcome = %v, want rejected", got)
	}
	if got := lines[2]["status"]; got != float64(http.StatusForbidden) {
		t.Fatalf("non-allowlisted audit status = %v, want 403", got)
	}
}

func TestSignCountersTrackOutcomes(t *testing.T) {
	signer, publicKey := newSigner(t)
	srv := server.New(signer, publicKey, server.Config{
		Allowlist:      server.Allowlist{"ok-vm"},
		CommitterName:  testCommitterName,
		CommitterEmail: testCommitterEmail,
		RateBurst:      1000,
	}, discardLogger())
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	commit := validCommit(testCommitterName, testCommitterEmail, "message\n")

	if resp, _ := postSignAs(t, ts.URL, "ok-vm", commit); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed request = %d, want 200", resp.StatusCode)
	}
	if resp, _ := postSignAs(t, ts.URL, "intruder", commit); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-allowlisted request = %d, want 403", resp.StatusCode)
	}
	if resp, _ := postSignAs(t, ts.URL, "", commit); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("identity-less request = %d, want 401", resp.StatusCode)
	}

	counters := srv.Stats()
	if counters.Signed != 1 || counters.Rejected != 2 || counters.Failed != 0 {
		t.Fatalf("counters = %+v, want signed=1 rejected=2 failed=0", counters)
	}

	// A broken signer yields a failed decision, counted separately.
	broken, err := signing.NewSSHKeygenSigner(signing.SSHKeygenConfig{KeyPath: filepath.Join(t.TempDir(), "missing")})
	if err != nil {
		t.Fatalf("NewSSHKeygenSigner: %v", err)
	}
	brokenSrv := server.New(broken, "ssh-ed25519 AAAA", server.Config{
		Allowlist:      server.Allowlist{"ok-vm"},
		CommitterName:  testCommitterName,
		CommitterEmail: testCommitterEmail,
		RateBurst:      1000,
	}, discardLogger())
	brokenTS := httptest.NewServer(brokenSrv)
	t.Cleanup(brokenTS.Close)

	if resp, _ := postSignAs(t, brokenTS.URL, "ok-vm", commit); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failing signer request = %d, want 500", resp.StatusCode)
	}
	if got := brokenSrv.Stats(); got.Failed != 1 || got.Signed != 0 {
		t.Fatalf("broken signer counters = %+v, want failed=1 signed=0", got)
	}
}
