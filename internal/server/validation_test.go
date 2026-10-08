package server_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
)

// The pinned committer identity used by the commit-validation tests.
const (
	testCommitterName  = "Test Committer"
	testCommitterEmail = "committer@example.com"
)

// commitConfig is a server config with the committer identity pinned.
func commitConfig() server.Config {
	return server.Config{CommitterName: testCommitterName, CommitterEmail: testCommitterEmail}
}

// validCommit builds a well-formed commit payload naming the given committer.
func validCommit(committerName, committerEmail, message string) []byte {
	return []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"parent 1111111111111111111111111111111111111111\n" +
		"author Author Person <author@example.com> 1700000000 +0000\n" +
		"committer " + committerName + " <" + committerEmail + "> 1700000000 +0000\n" +
		"\n" + message)
}

func TestSignRejectsPreExistingSignature(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, commitConfig())

	const prefix = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author Author Person <author@example.com> 1700000000 +0000\n" +
		"committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\n"

	cases := []struct {
		name    string
		payload string
	}{
		{"single-line gpgsig", prefix + "gpgsig -----BEGIN SSH SIGNATURE-----\n\nmessage\n"},
		{"multiline gpgsig", prefix +
			"gpgsig -----BEGIN SSH SIGNATURE-----\n" +
			" U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAg\n" +
			" AAAAQMUQMKCRzepWv86gHWChUGZk8DYfMgHBEx+FIlhE4cxFw5wMFJ6w\n" +
			" -----END SSH SIGNATURE-----\n" +
			"\nmessage\n"},
		{"gpgsig-sha256", prefix + "gpgsig-sha256 -----BEGIN SSH SIGNATURE-----\n\nmessage\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postSign(t, ts.URL, []byte(tc.payload))
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("POST already-signed commit status = %d, want 422: %s", resp.StatusCode, body)
			}
			if strings.Contains(body, "U1NIU0lH") {
				t.Fatalf("POST already-signed commit echoed signature bytes: %q", body)
			}
		})
	}
}

func TestSignRejectsCommitterMismatch(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, commitConfig())

	cases := []struct {
		name  string
		value string
	}{
		{"different name", "Other Person <" + testCommitterEmail + "> 1700000000 +0000"},
		{"different email", testCommitterName + " <other@example.com> 1700000000 +0000"},
		{"wrong case email", testCommitterName + " <COMMITTER@example.com> 1700000000 +0000"},
		{"display name suffixed", testCommitterName + " Jr <" + testCommitterEmail + "> 1700000000 +0000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
				"author Author Person <author@example.com> 1700000000 +0000\n" +
				"committer " + tc.value + "\n" +
				"\nmessage\n")
			resp, body := postSign(t, ts.URL, payload)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("POST committer mismatch status = %d, want 409: %s", resp.StatusCode, body)
			}
			if strings.Contains(body, tc.value) {
				t.Fatalf("POST committer mismatch echoed the payload: %q", body)
			}
		})
	}
}

// TestSignAllowsAuthorIdentityMismatch documents that only the committer is
// pinned: GitLab verifies the committer, and the author is explicitly out of
// scope.
func TestSignAllowsAuthorIdentityMismatch(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, commitConfig())

	payload := []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author Someone Else <someone-else@example.com> 1700000000 +0000\n" +
		"committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\n" +
		"\nmessage\n")

	resp, sig := postSign(t, ts.URL, payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST author-mismatch/committer-match status = %d, want 200: %s", resp.StatusCode, sig)
	}
}

func TestSignRejectsMalformedCommit(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, commitConfig())

	cases := []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"not a commit object", "this is not a commit object"},
		{"no header/message separator", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n"},
		{"continuation without a header", " leading space\ntree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n\nmessage\n"},
		{"header without a value", "tree\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n\nmessage\n"},
		{"invalid header key", "tr:ee 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n\nmessage\n"},
		{"missing tree", "author Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n\nmessage\n"},
		{"missing author", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n\nmessage\n"},
		{"missing committer", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\n\nmessage\n"},
		{"duplicate committer", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 +0000\n\nmessage\n"},
		{"committer without an email", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter No Email Here 1700000000 +0000\n\nmessage\n"},
		{"committer without a timestamp", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com>\n\nmessage\n"},
		{"committer with a bogus timezone", "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Author Person <author@example.com> 1700000000 +0000\ncommitter Test Committer <committer@example.com> 1700000000 Z\n\nmessage\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := postSign(t, ts.URL, []byte(tc.payload))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("POST malformed commit status = %d, want 400: %s", resp.StatusCode, body)
			}
			if tc.payload != "" && strings.Contains(body, tc.payload) {
				t.Fatalf("POST malformed commit echoed the payload: %q", body)
			}
		})
	}
}

// TestSignAcceptsAdversarialButValidPayloads locks in that only the header
// block is ever interpreted as headers: identity- or signature-shaped text
// inside the message body (or inside a folded header value) must not change
// the decision.
func TestSignAcceptsAdversarialButValidPayloads(t *testing.T) {
	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, commitConfig())

	// valid builds a commit with the pinned committer and extra headers/message.
	valid := func(headers, message string) []byte {
		return []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
			"author Author Person <author@example.com> 1700000000 +0000\n" +
			"committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\n" +
			headers + "\n" + message)
	}

	cases := []struct {
		name    string
		payload []byte
	}{
		{"fake committer header inside the message", valid("", "committer Evil Person <evil@example.com> 1700000000 +0000\n")},
		{"fake gpgsig header inside the message", valid("", "gpgsig -----BEGIN SSH SIGNATURE-----\n U1NIU0lH\n -----END SSH SIGNATURE-----\n")},
		{"fake headers after a blank line inside the message", valid("", "message\n\ntree deadbeef\ncommitter Evil <evil@example.com> 1 +0000\n")},
		{"committer lookalike folded into another header value", valid("mergetag object 1111111111111111111111111111111111111111\n committer Evil <evil@example.com> 1700000000 +0000\n", "message\n")},
		{"multiline mergetag header", valid("mergetag object 1111111111111111111111111111111111111111\n type commit\n tag v1.0\n\n tagger T <t@example.com> 1 +0000\n", "message\n")},
		{"extra encoding header", valid("encoding ISO-8859-1\n", "message\n")},
		{"non-UTF8 message bytes", valid("", string([]byte{0xff, 0xfe, 0x80, 'x', '\n'}))},
		{"empty message", valid("", "")},
		{"message without a trailing newline", valid("", "no trailing newline")},
		{"message that is only whitespace", valid("", "\n\n")},
		{"root commit without parents", []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
			"author Author Person <author@example.com> 1700000000 +0000\n" +
			"committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\n\ninitial\n")},
		{"multiple parents", []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
			"parent 1111111111111111111111111111111111111111\n" +
			"parent 2222222222222222222222222222222222222222\n" +
			"author Author Person <author@example.com> 1700000000 +0000\n" +
			"committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\n\nmerge\n")},
		{"negative timezone offset", []byte("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
			"author Author Person <author@example.com> 1700000000 -0730\n" +
			"committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 -0730\n\nmessage\n")},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, sig := postSign(t, ts.URL, tc.payload)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST adversarial-but-valid commit status = %d, want 200: %s", resp.StatusCode, sig)
			}
			if !strings.HasPrefix(sig, "-----BEGIN SSH SIGNATURE-----") {
				t.Fatalf("POST adversarial-but-valid commit body is not an SSHSIG: %q", sig)
			}
			// The payload must reach the signing backend byte-for-byte.
			if i == 0 || i == 6 {
				verifySignature(t, publicKey, tc.payload, sig)
			}
		})
	}
}

// TestSignRejectsValidCommitOverLimit checks the size boundary with a real
// commit payload rather than arbitrary bytes.
func TestSignRejectsValidCommitOverLimit(t *testing.T) {
	signer, publicKey := newSigner(t)
	commit := validCommit(testCommitterName, testCommitterEmail, "subject\n")
	cfg := commitConfig()
	cfg.MaxPayloadBytes = int64(len(commit) + 8) // room for the extra bytes below
	ts := newTestHTTPServer(t, signer, publicKey, cfg)

	// A well-formed commit whose message padding pushes it over the limit.
	over := validCommit(testCommitterName, testCommitterEmail, "subject\n"+strings.Repeat("padding ", 8))
	if int64(len(over)) <= cfg.MaxPayloadBytes {
		t.Fatalf("test payload %d bytes does not exceed limit %d", len(over), cfg.MaxPayloadBytes)
	}
	resp, body := postSign(t, ts.URL, over)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST oversized valid commit status = %d, want 413: %s", resp.StatusCode, body)
	}
}

// TestSignRejectionClassesUseDistinct4xxStatuses is the acceptance criterion
// that each rejection class has its own status code: malformed payload,
// oversized payload, pre-existing signature, and committer mismatch must all
// be distinguishable by status alone.
func TestSignRejectionClassesUseDistinct4xxStatuses(t *testing.T) {
	signer, publicKey := newSigner(t)
	cfg := commitConfig()
	cfg.MaxPayloadBytes = 4096
	ts := newTestHTTPServer(t, signer, publicKey, cfg)

	const prefix = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author Author Person <author@example.com> 1700000000 +0000\n"

	probes := []struct {
		name    string
		payload []byte
	}{
		{"oversized", bytes.Repeat([]byte("A"), 4097)},
		{"malformed", []byte("not a commit object")},
		{"pre-existing signature", []byte(prefix + "committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\ngpgsig x\n\nmessage\n")},
		{"committer mismatch", []byte(prefix + "committer Evil Person <evil@example.com> 1700000000 +0000\n\nmessage\n")},
	}

	seen := map[int]string{}
	for _, p := range probes {
		resp, body := postSign(t, ts.URL, p.payload)
		if resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Fatalf("POST %s status = %d, want a 4xx: %s", p.name, resp.StatusCode, body)
		}
		if other, ok := seen[resp.StatusCode]; ok {
			t.Fatalf("rejection classes %q and %q share status %d", other, p.name, resp.StatusCode)
		}
		seen[resp.StatusCode] = p.name
	}
}

// TestSignRejectionsLogReasonWithoutPayload checks the audit contract: every
// refusal logs a stable reason code and the payload hash, and never payload
// bytes.
func TestSignRejectionsLogReasonWithoutPayload(t *testing.T) {
	signer, publicKey := newSigner(t)

	const marker = "SUPERSECRETPAYLOADMARKER"

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	cfg := commitConfig()
	cfg.MaxPayloadBytes = 1024
	ts := httptest.NewServer(server.New(signer, publicKey, cfg, logger))
	t.Cleanup(ts.Close)

	const prefix = "tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\n" +
		"author Author Person <author@example.com> 1700000000 +0000\n"

	cases := []struct {
		name       string
		payload    []byte
		wantStatus int
		wantReason string
		// wantHash is false when the request is refused before the body is
		// read (declared Content-Length over the limit): there is no payload to
		// hash, only its declared size.
		wantHash bool
	}{
		{"malformed", []byte("tree " + marker + "\n"), http.StatusBadRequest, "malformed_commit", true},
		{"pre-existing signature", []byte(prefix + "committer " + testCommitterName + " <" + testCommitterEmail + "> 1700000000 +0000\ngpgsig " + marker + "\n\nmessage\n"), http.StatusUnprocessableEntity, "pre_existing_signature", true},
		{"committer mismatch", []byte(prefix + "committer " + marker + " <evil@example.com> 1700000000 +0000\n\nmessage\n"), http.StatusConflict, "committer_mismatch", true},
		{"oversized", bytes.Repeat([]byte(marker), 128), http.StatusRequestEntityTooLarge, "payload_too_large", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf.Reset()
			resp, body := postSign(t, ts.URL, tc.payload)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("POST %s status = %d, want %d: %s", tc.name, resp.StatusCode, tc.wantStatus, body)
			}

			logged := buf.String()
			if logged == "" {
				t.Fatal("no audit log line was emitted for the rejection")
			}
			if !strings.Contains(logged, `"reason":"`+tc.wantReason+`"`) {
				t.Fatalf("audit log does not carry reason %q: %s", tc.wantReason, logged)
			}
			if tc.wantHash && !strings.Contains(logged, `"payload_sha256"`) {
				t.Fatalf("audit log does not carry the payload hash: %s", logged)
			}
			if !tc.wantHash && !strings.Contains(logged, `"payload_bytes"`) {
				t.Fatalf("audit log carries neither a hash nor a size: %s", logged)
			}
			if strings.Contains(logged, marker) || strings.Contains(body, marker) {
				t.Fatalf("rejection leaked payload bytes; log %q body %q", logged, body)
			}
		})
	}

	// The success path logs the hash and no payload either.
	buf.Reset()
	commit := validCommit(testCommitterName, testCommitterEmail, "message with "+marker+" inside\n")
	resp, _ := postSign(t, ts.URL, commit)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST valid commit status = %d, want 200", resp.StatusCode)
	}
	logged := buf.String()
	if !strings.Contains(logged, `"payload_sha256"`) {
		t.Fatalf("success audit log does not carry the payload hash: %s", logged)
	}
	if strings.Contains(logged, marker) {
		t.Fatalf("success audit log leaked payload bytes: %s", logged)
	}
}

// TestSignRealCommitPayload signs the exact bytes of a commit object created
// by the real git binary, which is the happy path the client drives.
func TestSignRealCommitPayload(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available:", err)
	}

	signer, publicKey := newSigner(t)
	ts := newTestHTTPServer(t, signer, publicKey, commitConfig())

	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	run("init", "-q")
	run("config", "user.name", testCommitterName)
	run("config", "user.email", testCommitterEmail)
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("content\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	run("add", "file.txt")
	run("commit", "-q", "-m", "real commit subject")

	payload := []byte(run("cat-file", "commit", "HEAD"))
	if !bytes.Contains(payload, []byte("committer ")) {
		t.Fatalf("git cat-file output does not look like a commit object: %q", payload)
	}

	resp, sig := postSign(t, ts.URL, payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST real commit payload status = %d, want 200: %s", resp.StatusCode, sig)
	}
	verifySignature(t, publicKey, payload, sig)
}
