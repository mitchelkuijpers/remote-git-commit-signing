package server

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/wire"
)

// This file implements client distribution: GET /install.sh renders a
// bootstrap script and GET /v1/client/{name} serves the files it downloads
// (git-remote-sign builds and install-client.sh). Provisioning an agent VM
// then reduces to `curl <signer>/install.sh | sh`. Both endpoints are
// unauthenticated, same exposure class as the landing page: they carry no key
// material, only the public key and already-public configuration.

// installTemplate is the bootstrap script served at /install.sh. It downloads
// the one client binary for the caller's architecture plus install-client.sh
// from this signer, marks the binary executable, and execs the installer with
// the pinned configuration as environment. The script itself holds no setup
// logic worth drifting: everything real stays in the tested
// deploy/install-client.sh, and both the architecture dispatch and the
// downloaded artifact names come from the clientFiles whitelist — the same set
// GET /v1/client/{name} serves — so a whitelist edit cannot strand the
// bootstrap with a 404 or a silent no-op. Values are shell-quoted in
// handleInstallScript before rendering.
var installTemplate = template.Must(template.New("install.sh").Parse(`#!/bin/sh
# git-remote-sign client bootstrap, rendered by the signer. Provisions this VM
# as a signing client. Safe to re-run.
set -eu

base={{.SignerURL}}

# Exactly one binary, chosen by the caller's architecture. The dispatch arms are
# derived from the client-distribution whitelist, so an architecture the signer
# does not serve fails closed here instead of fetching nothing or the wrong
# variant.
case "$(uname -m)" in
{{range .ArchCases}}{{.Machines}}) arch={{.Arch}} ;;
{{end}}*) echo "install.sh: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "install.sh: downloading git-remote-sign (linux/$arch) and the installer from $base"
curl -fsSL "$base/v1/client/{{.ClientBinaryPrefix}}$arch" -o "$tmp/{{.ClientBinaryName}}"
chmod 0755 "$tmp/{{.ClientBinaryName}}"
{{range .ClientArtifacts}}curl -fsSL "$base/v1/client/{{.Name}}" -o "$tmp/{{.Dest}}"
{{end}}
SIGNER_URL=$base \
SIGNER_PUBLIC_KEY={{.PublicKey}} \
SIGNER_COMMITTER_NAME={{.CommitterName}} \
SIGNER_COMMITTER_EMAIL={{.CommitterEmail}} \
GIT_REMOTE_SIGNER_BIN="$tmp/{{.ClientBinaryName}}" \
sh "$tmp/{{.InstallClientName}}"
`))

// Artifact naming under the whitelist: the bootstrap saves the chosen client
// binary to one working path, and the installer script keeps its own name. A
// whitelist key must match one of these shapes to be downloaded.
const (
	clientBinaryPrefix      = "git-remote-sign"
	clientBinaryLinuxPrefix = clientBinaryPrefix + "-linux-" // served client binary name prefix
	clientBinaryName        = clientBinaryPrefix             // working name inside the bootstrap's tmp dir
	installClientName       = "install-client.sh"
	// shellScriptContentType is the response media type of the installer
	// script; it is deliberately not a wire constant, the wire contract only
	// pins the payload and signature media types.
	shellScriptContentType = "text/x-shellscript; charset=utf-8"
)

// clientMachines maps the architecture suffix of a served client binary to the
// uname -m spellings that select it. It carries the platform knowledge the
// whitelist cannot: which machines a served variant runs on.
var clientMachines = map[string]string{
	"amd64": "x86_64",
	"arm64": "aarch64 | arm64",
}

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
		SignerURL          string
		PublicKey          string
		CommitterName      string
		CommitterEmail     string
		ClientBinaryPrefix string
		ClientBinaryName   string
		InstallClientName  string
		ArchCases          []clientArchCase
		ClientArtifacts    []clientArtifact
	}{
		SignerURL:          shellQuote(s.signerURL),
		PublicKey:          shellQuote(strings.TrimSpace(s.publicKey)),
		CommitterName:      shellQuote(s.committer.Name),
		CommitterEmail:     shellQuote(s.committer.Email),
		ClientBinaryPrefix: clientBinaryLinuxPrefix,
		ClientBinaryName:   clientBinaryName,
		InstallClientName:  installClientName,
		ArchCases:          clientArchCases(),
		ClientArtifacts:    clientArtifacts(),
	})
	if err != nil {
		s.logger.Error("render install.sh", "error", err)
	}
}

// clientArtifact is one download the bootstrap script renders: the whitelisted
// name under /v1/client/ and the path the script saves it to.
type clientArtifact struct {
	Name string
	Dest string
}

// clientArchCase is one arm of the bootstrap's uname -m dispatch: the machine
// spellings that select an architecture and the architecture they name.
type clientArchCase struct {
	Machines string
	Arch     string
}

// clientFiles is the whitelist of files GET /v1/client/{name} serves from the
// dist directory: the exact set the bootstrap script downloads. Anything else
// in the directory is not exposed, and no path traversal is possible because
// the name is matched against this fixed set, never spliced into a path. The
// bootstrap derives both its architecture dispatch and its artifact downloads
// from these keys, so this whitelist is the single owner of the artifact set.
var clientFiles = map[string]string{
	"git-remote-sign-linux-amd64": wire.ContentTypeOctetStream,
	"git-remote-sign-linux-arm64": wire.ContentTypeOctetStream,
	"install-client.sh":           shellScriptContentType,
}

// clientArchCases derives the bootstrap's architecture dispatch from the
// clientFiles whitelist: only an architecture the signer actually serves gets
// an arm, so the whitelist stays the single owner of the artifact set and
// adding an architecture stays a one-place change. An architecture the caller
// reports that has no served binary falls through to the fail-closed default
// arm; a whitelisted binary whose architecture has no known machine spelling
// is not selectable and gets no arm.
func clientArchCases() []clientArchCase {
	var cases []clientArchCase
	for name := range clientFiles {
		arch, ok := strings.CutPrefix(name, clientBinaryLinuxPrefix)
		if !ok {
			continue
		}
		machines, ok := clientMachines[arch]
		if !ok {
			continue
		}
		cases = append(cases, clientArchCase{Machines: machines, Arch: arch})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Arch < cases[j].Arch })
	return cases
}

// clientArtifacts maps the whitelisted files that are not architecture
// variants — the installer script — to the download the bootstrap renders for
// them, so the bootstrap downloads every served artifact and nothing else. A
// whitelist key matching no shape renders no download: the bootstrap and the
// served set cannot drift apart silently.
func clientArtifacts() []clientArtifact {
	var artifacts []clientArtifact
	for name := range clientFiles {
		if name == installClientName {
			artifacts = append(artifacts, clientArtifact{Name: name, Dest: installClientName})
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return artifacts
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
