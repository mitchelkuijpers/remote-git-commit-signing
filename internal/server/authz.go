package server

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/wire"
)

// StampSourceVM stamps the platform-verified source-VM identity onto a request.
// In production only the exe.dev authenticated peer proxy may set
// wire.SourceVMHeader, and nothing else ever should; this helper exists so the
// test harnesses can present requests exactly as the transparent platform
// does. The signing middleware trusts a stamped identity as platform-verified.
func StampSourceVM(h http.Header, vm string) {
	h.Set(wire.SourceVMHeader, vm)
}

// Outcome values recorded in the audit log and counters.
const (
	outcomeSigned   = "signed"
	outcomeRejected = "rejected"
	outcomeFailed   = "failed"
)

// Allowlist is the set of VM identities permitted to sign. Each entry is
// either an exact VM name or a path.Match glob pattern (for example
// "agent-*"). An empty Allowlist admits nobody: authorization is fail-closed.
type Allowlist []string

// Matches reports whether vm is permitted by the allowlist.
func (a Allowlist) Matches(vm string) bool {
	for _, pattern := range a {
		if pattern == vm {
			return true
		}
		if ok, err := path.Match(pattern, vm); err == nil && ok {
			return true
		}
	}
	return false
}

// decision carries per-request audit state from the signing handler back to
// the authorization middleware, which emits exactly one audit line.
type decision struct {
	vm            string
	requestID     string
	payloadSHA256 string
	payloadBytes  int
	// reason is a stable rejection-reason code (never payload bytes) set by
	// the handler when it refuses a payload.
	reason string
	err    error
}

// decisionKey is the context key under which a decision is stored.
type decisionKey struct{}

// decisionFrom returns the decision stored on ctx, or nil when absent.
func decisionFrom(ctx context.Context) *decision {
	d, _ := ctx.Value(decisionKey{}).(*decision)
	return d
}

// withAuthorization guards a signing handler with the identity policy chain:
// a verified source-VM identity is required (401 when absent), the identity
// must be allowlisted (403), and its per-VM rate limit must not be exhausted
// (429). It then emits exactly one audit log line for the decision, carrying
// the VM, payload hash, status and duration. Payload bytes are never logged.
func (s *Server) withAuthorization(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		d := &decision{
			vm:        strings.TrimSpace(r.Header.Get(wire.SourceVMHeader)),
			requestID: requestIDFrom(r.Context()),
		}
		r = r.WithContext(context.WithValue(r.Context(), decisionKey{}, d))

		sw := &statusWriter{ResponseWriter: w}
		switch {
		case d.vm == "":
			writeText(sw, http.StatusUnauthorized, "missing VM identity")
		case !s.allowlist.Matches(d.vm):
			writeText(sw, http.StatusForbidden, "VM identity not allowed")
		case !s.limiter.allow(d.vm, start):
			writeText(sw, http.StatusTooManyRequests, "rate limit exceeded")
		default:
			next(sw, r)
		}

		s.recordDecision(d, sw.status, time.Since(start))
	}
}

// statusWriter records the response status while passing writes through.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Counters are the in-process signing decision counts.
type Counters struct {
	Signed   int64
	Rejected int64
	Failed   int64
}

// Stats returns a snapshot of the in-process decision counters.
func (s *Server) Stats() Counters {
	return Counters{Signed: s.signed.Load(), Rejected: s.rejected.Load(), Failed: s.failed.Load()}
}

// recordDecision increments the outcome counter and emits the single audit
// line for a signing decision.
func (s *Server) recordDecision(d *decision, status int, elapsed time.Duration) {
	if status == 0 {
		status = http.StatusOK
	}

	level, outcome := slog.LevelInfo, outcomeSigned
	switch {
	case status >= 500:
		level, outcome = slog.LevelError, outcomeFailed
	case status >= 400:
		level, outcome = slog.LevelWarn, outcomeRejected
	}

	switch outcome {
	case outcomeSigned:
		s.signed.Add(1)
	case outcomeRejected:
		s.rejected.Add(1)
	default:
		s.failed.Add(1)
	}

	attrs := []any{
		"event", "sign_request",
		"outcome", outcome,
		"vm", d.vm,
		"request_id", d.requestID,
		"payload_sha256", d.payloadSHA256,
		"payload_bytes", d.payloadBytes,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	}
	if d.err != nil {
		attrs = append(attrs, "error", d.err.Error())
	}
	if d.reason != "" {
		attrs = append(attrs, "reason", d.reason)
	}
	s.logger.Log(context.Background(), level, "signing decision", attrs...)
}
