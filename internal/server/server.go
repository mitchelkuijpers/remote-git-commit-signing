// Package server exposes the HTTP API of git-signer-server: the signing
// endpoint plus the public-key, liveness, and readiness probes.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// Server is the HTTP handler set of git-signer-server.
//
// A Server with an empty public key (and/or a nil signer) is not ready: it
// answers liveness but reports 503 on readiness and refuses to sign.
type Server struct {
	signer      signing.Signer
	publicKey   string
	maxPayload  int64
	signTimeout time.Duration
	// committerName and committerEmail are the pinned identity a commit must
	// name to be signed.
	committerName  string
	committerEmail string
	logger         *slog.Logger
	mux            *http.ServeMux
	allowlist      Allowlist
	limiter        *rateLimiter
	signed         atomic.Int64
	rejected       atomic.Int64
	failed         atomic.Int64
}

// New builds the HTTP handler. publicKey is the derived public signing key; it
// is empty when no key is loaded, which makes the server report not-ready.
func New(signer signing.Signer, publicKey string, cfg Config, logger *slog.Logger) *Server {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		signer:         signer,
		publicKey:      publicKey,
		maxPayload:     cfg.MaxPayloadBytes,
		signTimeout:    cfg.SignTimeout,
		committerName:  cfg.CommitterName,
		committerEmail: cfg.CommitterEmail,
		logger:         logger,
		allowlist:      cfg.Allowlist,
		limiter:        newRateLimiter(cfg.RatePerMin, cfg.RateBurst),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sign", s.withAuthorization(s.handleSign))
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /v1/public-key", s.handlePublicKey)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	s.mux = mux
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Ready reports whether the signing key is loaded and the server can sign.
func (s *Server) Ready() bool {
	return s.publicKey != "" && s.signer != nil
}

// handleRoot serves the public landing page. Like the probes it exposes no
// key material or configuration and requires no identity.
func (s *Server) handleRoot(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "git-signer-server: POST /v1/sign")
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if !s.Ready() {
		writeText(w, http.StatusServiceUnavailable, "signing key not loaded")
		return
	}
	writeText(w, http.StatusOK, "ok")
}

// contentTypeSSHSig is the response media type for a raw SSHSIG signature.
const contentTypeSSHSig = "application/vnd.sshsig"

// handlePublicKey serves the derived public signing key. It is informational:
// clients pin the key and must not blindly trust a fetched one.
func (s *Server) handlePublicKey(w http.ResponseWriter, _ *http.Request) {
	if !s.Ready() {
		writeText(w, http.StatusServiceUnavailable, "signing key not loaded")
		return
	}
	writeText(w, http.StatusOK, s.publicKey)
}

// handleSign signs the request body. The body must be a git commit object
// naming the configured committer identity and must not already carry a
// signature; anything else is refused with a distinct 4xx before it reaches
// the signing backend. The authorization middleware has already enforced
// identity, allowlist and rate limit, and owns the single audit log line for
// this decision — including the rejection reason recorded here.
func (s *Server) handleSign(w http.ResponseWriter, r *http.Request) {
	if !s.Ready() {
		writeText(w, http.StatusServiceUnavailable, "signing key not loaded")
		return
	}

	// Reject early when the declared size already exceeds the limit.
	if r.ContentLength > s.maxPayload {
		noteRejection(r, reasonPayloadTooLarge, r.ContentLength)
		writeText(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, s.maxPayload+1))
	if err != nil {
		writeText(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if int64(len(payload)) > s.maxPayload {
		noteRejection(r, reasonPayloadTooLarge, -1)
		writeText(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}

	// Publish the payload hash for the audit line. The payload itself never
	// leaves this process, and only its hash is logged.
	if d := decisionFrom(r.Context()); d != nil {
		d.payloadSHA256 = payloadHash(payload)
		d.payloadBytes = len(payload)
	}

	// Commit-only: the payload must be a commit object naming the configured
	// committer and must not already carry a signature, before it reaches the
	// signing backend.
	if check := s.checkCommit(payload); check.status != 0 {
		noteRejection(r, check.reason, -1)
		writeText(w, check.status, check.message)
		return
	}

	// Per-request signing deadline, on top of the server's own timeouts.
	ctx, cancel := context.WithTimeout(r.Context(), s.signTimeout)
	defer cancel()

	sig, err := s.signer.Sign(ctx, payload)
	if err != nil {
		if errors.Is(err, signing.ErrPayloadTooLarge) {
			noteRejection(r, reasonPayloadTooLarge, -1)
			writeText(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		if d := decisionFrom(r.Context()); d != nil {
			d.err = err
		}
		writeText(w, http.StatusInternalServerError, "signing failed")
		return
	}

	w.Header().Set("Content-Type", contentTypeSSHSig)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sig)
}

// payloadHash returns the hex SHA-256 of the payload for audit logging. The
// payload itself is never logged.
func payloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// writeText writes a plain-text response with the given status.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}
