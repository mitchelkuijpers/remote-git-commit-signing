package server

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

// landingTemplate is the static, dependency-free landing page served at "/". It
// carries no JavaScript and no external assets: everything needed to register
// the signing key is inline. Only the public key line and its fingerprint are
// interpolated; html/template escapes both.
var landingTemplate = template.Must(template.New("landing").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Git commit signing service</title>
<style>
body { font-family: system-ui, sans-serif; line-height: 1.5; max-width: 46rem; margin: 2rem auto; padding: 0 1rem; color: #1f2328; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.875rem; }
pre { background: #f6f8fa; border: 1px solid #d0d7de; border-radius: 6px; padding: 0.75rem; overflow-x: auto; }
h1 { font-size: 1.5rem; }
h2 { font-size: 1.15rem; margin-top: 1.75rem; }
</style>
</head>
<body>
<h1>Git commit signing service</h1>
<p>
This service signs Git commits on behalf of VMs using one shared SSH signing
key. Register the public key below with GitLab as a <strong>Signing</strong>
key to have those commits show as verified.
</p>

<h2>Public signing key</h2>
<pre>{{.PublicKey}}</pre>
<p>Fingerprint (SHA256): <code>{{.Fingerprint}}</code></p>

<h2>Set up an agent VM</h2>
<p>
On any VM attached to the signer&rsquo;s peer integration, provision the client
with one command (idempotent, safe to re-run):
</p>
<pre>curl -fsSL {{.PublicURL}}/install.sh | sh</pre>
<p>
The script downloads the client and installer from this signer and configures
Git to sign every commit through it. No private key is ever stored on the
VM.
</p>

<h2>Register with GitLab</h2>
<ol>
<li>Sign in to GitLab and open your avatar &rarr; <em>Edit profile</em>
(<em>Preferences</em> &rarr; <em>SSH Keys</em>).</li>
<li>Paste the public key line above into the <em>Key</em> box.</li>
<li>Set <em>Usage type</em> to <strong>Signing</strong>. Do not use
<em>Authentication</em> or <em>Authentication &amp; Signing</em>: this key only
signs commits.</li>
<li>Give it a recognizable title, then click <em>Add key</em>.</li>
</ol>

<h2>Verify a commit</h2>
<p>
After the key is registered, signed commits are marked verified in GitLab. Locally
you can check them with <code>git log --show-signature</code>, or verify a single
commit with <code>git verify-commit &lt;commit&gt;</code>.
</p>
</body>
</html>
`))

// handleLandingPage serves the read-only HTML landing page. It exposes only
// public information — the public key, its fingerprint, and the public
// bootstrap URL: never the key path, environment, or any other configuration.
func (s *Server) handleLandingPage(w http.ResponseWriter, _ *http.Request) {
	if !s.Ready() {
		writeText(w, http.StatusServiceUnavailable, "signing key not loaded")
		return
	}

	fingerprint, err := sshFingerprint(s.publicKey)
	if err != nil {
		s.logger.Error("landing page rendering failed",
			"event", "landing_page", "status", "fingerprint_failed", "error", err.Error())
		writeText(w, http.StatusInternalServerError, "landing page unavailable")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if err := landingTemplate.Execute(w, struct {
		PublicKey   string
		Fingerprint string
		PublicURL   string
	}{PublicKey: strings.TrimSpace(s.publicKey), Fingerprint: fingerprint, PublicURL: s.publicURL}); err != nil {
		// The header is already written; only the log can record the failure.
		s.logger.Error("landing page rendering failed",
			"event", "landing_page", "status", "template_failed", "error", err.Error())
	}
}

// sshFingerprint derives the OpenSSH SHA256 fingerprint ("SHA256:<base64>") of an
// authorized_keys-format public key line. It is computed with the standard
// library over the base64-decoded key blob, matching `ssh-keygen -l -E sha256`.
func sshFingerprint(publicKeyLine string) (string, error) {
	fields := strings.Fields(publicKeyLine)
	if len(fields) < 2 {
		return "", fmt.Errorf("malformed public key line: %q", publicKeyLine)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", fmt.Errorf("decode public key blob: %w", err)
	}
	if len(blob) == 0 {
		return "", errors.New("empty public key blob")
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}
