package server_test

import (
	"net/http"
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
