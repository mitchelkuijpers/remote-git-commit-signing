package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// This file implements client distribution: GET /install.sh renders a
// bootstrap script and GET /v1/client/{name} serves the files it downloads
// (git-remote-sign builds and install-client.sh). Provisioning an agent VM
// then reduces to `curl <signer>/install.sh | sh`. Both endpoints are
// unauthenticated, same exposure class as the landing page: they carry no key
// material, only the public key and already-public configuration.

// installTemplate is the bootstrap script served at /install.sh. It downloads
// the client binary and install-client.sh from this signer and execs the
// installer with the pinned configuration as environment. The script itself
// holds no setup logic worth drifting: everything real stays in the tested
// deploy/install-client.sh. Values are shell-quoted in handleInstallScript
// before rendering.
var installTemplate = template.Must(template.New("install.sh").Parse(`#!/bin/sh
# git-remote-sign client bootstrap, rendered by the signer. Provisions this VM
# as a signing client. Safe to re-run.
set -eu

base={{.PublicURL}}

case "$(uname -m)" in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "install.sh: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "install.sh: downloading git-remote-sign (linux/$arch) and the installer from $base"
curl -fsSL "$base/v1/client/git-remote-sign-linux-$arch" -o "$tmp/git-remote-sign"
curl -fsSL "$base/v1/client/install-client.sh" -o "$tmp/install-client.sh"
chmod +x "$tmp/git-remote-sign"

GIT_REMOTE_SIGNER_URL=$base \
GIT_REMOTE_SIGNER_PUBLIC_KEY={{.PublicKey}} \
GIT_SIGNER_COMMITTER_NAME={{.CommitterName}} \
GIT_SIGNER_COMMITTER_EMAIL={{.CommitterEmail}} \
GIT_REMOTE_SIGNER_BIN="$tmp/git-remote-sign" \
sh "$tmp/install-client.sh"
`))

// shellQuote renders s as a POSIX single-quoted string literal.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// installRenderable reports whether the bootstrap script can do its job: the
// server must be ready (it renders the public key) and a dist directory must
// be configured (the script downloads from /v1/client/...).
func (s *Server) installRenderable() bool {
	return s.Ready() && s.distDir != ""
}

// handleInstallScript serves the rendered client bootstrap script.
func (s *Server) handleInstallScript(w http.ResponseWriter, _ *http.Request) {
	if !s.installRenderable() {
		writeText(w, http.StatusServiceUnavailable, "client bootstrap unavailable: signing key or dist directory not configured")
		return
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	err := installTemplate.Execute(w, struct {
		PublicURL      string
		PublicKey      string
		CommitterName  string
		CommitterEmail string
	}{
		PublicURL:      shellQuote(s.publicURL),
		PublicKey:      shellQuote(strings.TrimSpace(s.publicKey)),
		CommitterName:  shellQuote(s.committer.Name),
		CommitterEmail: shellQuote(s.committer.Email),
	})
	if err != nil {
		s.logger.Error("render install.sh", "error", err)
	}
}

// clientFiles is the whitelist of files GET /v1/client/{name} serves from the
// dist directory: the exact set the bootstrap script downloads. Anything else
// in the directory is not exposed, and no path traversal is possible because
// the name is matched against this fixed set, never spliced into a path.
var clientFiles = map[string]string{
	"git-remote-sign-linux-amd64": "application/octet-stream",
	"git-remote-sign-linux-arm64": "application/octet-stream",
	"install-client.sh":           "text/x-shellscript; charset=utf-8",
}

// handleClientFile serves one whitelisted file from the dist directory.
func (s *Server) handleClientFile(w http.ResponseWriter, r *http.Request) {
	contentType, ok := clientFiles[r.PathValue("name")]
	if !ok || s.distDir == "" {
		writeText(w, http.StatusNotFound, "unknown client file")
		return
	}

	path := filepath.Join(s.distDir, r.PathValue("name"))
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		writeText(w, http.StatusNotFound, "client file not installed on the signer")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}
