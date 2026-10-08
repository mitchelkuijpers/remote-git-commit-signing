// Package server exposes the HTTP API of git-signer-server: the signing
// endpoint plus the public-key, liveness, and readiness probes.
package server

import (
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
	logger      *slog.Logger
	mux         *http.ServeMux
}

// New builds the HTTP handler. publicKey is the derived public signing key; it
// is empty when no key is loaded, which makes the server report not-ready.
func New(signer signing.Signer, publicKey string, cfg Config, logger *slog.Logger) *Server {
	cfg = cfg.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		signer:      signer,
		publicKey:   publicKey,
		maxPayload:  cfg.MaxPayloadBytes,
		signTimeout: cfg.SignTimeout,
		logger:      logger,
	}

	mux := http.NewServeMux()
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

// writeText writes a plain-text response with the given status.
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}
