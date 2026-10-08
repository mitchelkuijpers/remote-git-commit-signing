package server_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
)

// TestRequestIDOnResponseAndAuditLine covers the per-request identifier: every
// response carries X-Request-Id, each request gets a fresh value, and the
// signing audit line records the same identifier as the response header.
func TestRequestIDOnResponseAndAuditLine(t *testing.T) {
	signer, publicKey := newSigner(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	srv := server.New(signer, publicKey, signConfig(), logger)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	payload := validCommit(testCommitterName, testCommitterEmail, "subject\n")
	resp, _ := postSign(t, ts.URL, payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/sign status = %d, want 200", resp.StatusCode)
	}
	headerID := resp.Header.Get("X-Request-Id")
	if headerID == "" {
		t.Fatal("response is missing the X-Request-Id header")
	}

	lines := logLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1: %v", len(lines), lines)
	}
	if got := lines[0]["request_id"]; got != headerID {
		t.Fatalf("audit request_id = %v, want the response header %q", got, headerID)
	}

	// Rejections also carry an id, and every request gets a distinct one.
	resp2, _ := get(t, ts.URL+"/healthz")
	if got := resp2.Header.Get("X-Request-Id"); got == "" || got == headerID {
		t.Fatalf("second response X-Request-Id = %q, want a fresh distinct id", got)
	}
}
