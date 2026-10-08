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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sign", s.handleSign)
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

// handleSign signs the request body. The body is an opaque octet-stream: this
// slice signs whatever it is given, and commit validation lands later.
func (s *Server) handleSign(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	elapsed := func() int64 { return time.Since(start).Milliseconds() }

	if !s.Ready() {
		s.logger.Warn("sign request refused",
			"event", "sign_request", "status", "not_ready", "duration_ms", elapsed())
		writeText(w, http.StatusServiceUnavailable, "signing key not loaded")
		return
	}

	// Reject early when the declared size already exceeds the limit.
	if r.ContentLength > s.maxPayload {
		s.logger.Warn("sign request refused",
			"event", "sign_request", "status", "payload_too_large", "reason", reasonPayloadTooLarge,
			"payload_bytes", r.ContentLength, "duration_ms", elapsed())
		writeText(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, s.maxPayload+1))
	if err != nil {
		s.logger.Warn("sign request refused",
			"event", "sign_request", "status", "unreadable_body", "duration_ms", elapsed())
		writeText(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if int64(len(payload)) > s.maxPayload {
		s.logger.Warn("sign request refused",
			"event", "sign_request", "status", "payload_too_large", "reason", reasonPayloadTooLarge,
			"payload_bytes", len(payload), "duration_ms", elapsed())
		writeText(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}

	// Commit-only: the payload must be a commit object naming the configured
	// committer, unsigned, before it reaches the signing backend.
	if check := s.checkCommit(payload); check.status != 0 {
		s.logger.Warn("sign request refused",
			"event", "sign_request", "status", "rejected", "reason", check.reason,
			"payload_sha256", payloadHash(payload), "payload_bytes", len(payload),
			"duration_ms", elapsed())
		writeText(w, check.status, check.message)
		return
	}

	// Per-request signing deadline, on top of the server's own timeouts.
	ctx, cancel := context.WithTimeout(r.Context(), s.signTimeout)
	defer cancel()

	sig, err := s.signer.Sign(ctx, payload)
	if err != nil {
		if errors.Is(err, signing.ErrPayloadTooLarge) {
			s.logger.Warn("sign request refused",
				"event", "sign_request", "status", "payload_too_large", "reason", reasonPayloadTooLarge,
				"payload_bytes", len(payload), "duration_ms", elapsed())
			writeText(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		s.logger.Error("sign request failed",
			"event", "sign_request", "status", "signing_failed",
			"error", err.Error(), "duration_ms", elapsed())
		writeText(w, http.StatusInternalServerError, "signing failed")
		return
	}

	s.logger.Info("sign request succeeded",
		"event", "sign_request", "status", "success",
		"payload_sha256", payloadHash(payload), "payload_bytes", len(payload),
		"duration_ms", elapsed())

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
