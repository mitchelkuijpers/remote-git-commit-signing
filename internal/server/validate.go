package server

import (
	"net/http"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/gitobj"
)

// Rejection reason codes. They are the audit-log metadata emitted for every
// refused signing request; the payload itself is never logged.
const (
	reasonMalformedCommit   = "malformed_commit"
	reasonPreExistingSig    = "pre_existing_signature"
	reasonCommitterMismatch = "committer_mismatch"
)

// commitCheck is the outcome of validating a signing payload as a commit
// object whose committer is the configured identity. A zero status means the
// payload is acceptable.
type commitCheck struct {
	status  int
	reason  string // audit-log metadata
	message string // client-facing explanation
}

// checkCommit validates payload as a commit object before it reaches the
// signing backend. Rejection classes are distinct so callers can react
// differently: malformed (400), already signed (422), and a committer that is
// not the configured identity (409). Oversized payloads are rejected by the
// caller before parsing.
func (s *Server) checkCommit(payload []byte) commitCheck {
	commit, err := gitobj.ParseCommit(payload)
	if err != nil {
		// The parse error is deliberately not logged or returned: it can quote
		// payload bytes. The reason code is enough for the audit trail.
		return commitCheck{
			status:  http.StatusBadRequest,
			reason:  reasonMalformedCommit,
			message: "malformed commit payload",
		}
	}

	// A payload git handed to the signing program never carries its own
	// signature; one means the request is an attempt to re-sign or to smuggle
	// arbitrary signed content.
	if commit.HasHeader("gpgsig") || commit.HasHeader("gpgsig-sha256") {
		return commitCheck{
			status:  http.StatusUnprocessableEntity,
			reason:  reasonPreExistingSig,
			message: "commit already carries a signature",
		}
	}

	raw, ok := commit.Header("committer")
	if !ok {
		// ParseCommit guarantees a committer header; keep the check honest.
		return commitCheck{
			status:  http.StatusBadRequest,
			reason:  reasonMalformedCommit,
			message: "malformed commit payload",
		}
	}
	sig, err := gitobj.ParseSignature(raw)
	if err != nil {
		return commitCheck{
			status:  http.StatusBadRequest,
			reason:  reasonMalformedCommit,
			message: "malformed commit payload",
		}
	}

	// The committer identity is pinned: GitLab verifies the committer email,
	// so anything else must never be signed with this key. The author's
	// identity is deliberately not checked.
	if sig.Name != s.committerName || sig.Email != s.committerEmail {
		return commitCheck{
			status:  http.StatusConflict,
			reason:  reasonCommitterMismatch,
			message: "committer identity is not permitted",
		}
	}

	return commitCheck{}
}
