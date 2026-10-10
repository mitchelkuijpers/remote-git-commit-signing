// Package setting owns the environment names of the deployment interface: the
// single SIGNER_* family shared by git-signer-server, git-remote-sign, the
// deploy scripts, the systemd unit, and the documentation. Config loading in
// internal/server and internal/client consumes these constants, and a drift
// test asserts that the shell scripts, the unit file, and the docs spell the
// exact names defined here.
//
// The names are the interface; behavior (validation, defaulting) stays with
// the packages that read the environment. The only value default defined here
// is DefaultSignerURL — the one default that shell/doc copies previously
// drifted out of sync with.
//
// Deliberately outside the family: the one-shot override knobs of
// install-client.sh itself (GIT_REMOTE_SIGNER_BIN,
// GIT_REMOTE_SIGNER_RELEASE_VERSION, GIT_REMOTE_SIGNER_DOWNLOAD_BASE,
// GIT_REMOTE_SIGNER_INSTALL_DIR, GIT_REMOTE_SIGNER_CONFIG_DIR,
// GIT_REMOTE_SIGNER_PROFILE). They configure the installer run, are persisted
// nowhere, and are recorded as out of scope in
// docs/adr/0001-one-signer-env-family.md.
package setting

// Server runtime settings read by server.LoadConfig.
const (
	// SignerKeyPath is the path to the private signing key. Required.
	SignerKeyPath = "SIGNER_KEY_PATH"
	// SignerPort is the TCP listen port. Defaults to server.DefaultPort.
	SignerPort = "SIGNER_PORT"
	// SignerCommitterName is the pinned committer name. Required.
	SignerCommitterName = "SIGNER_COMMITTER_NAME"
	// SignerCommitterEmail is the pinned committer email. Required.
	SignerCommitterEmail = "SIGNER_COMMITTER_EMAIL"
	// SignerAllowlist is the fail-closed source-VM identity allowlist.
	// Required in practice: unset admits nobody.
	SignerAllowlist = "SIGNER_ALLOWLIST"
	// SignerRatePerMin is the sustained per-VM signing rate.
	SignerRatePerMin = "SIGNER_RATE_PER_MIN"
	// SignerRateBurst is the per-VM burst capacity.
	SignerRateBurst = "SIGNER_RATE_BURST"
	// SignerDistDir is the client-distribution directory the server serves
	// (empty disables /install.sh downloads and /v1/client/...).
	SignerDistDir = "SIGNER_DIST_DIR"
)

// The signer base URL and the client settings. SignerURL is one concept at two
// ends of the signing seam: the server renders its value into the client
// bootstrap, and the client connects to it. SignerPublicKey and SignerTimeout
// configure the client.
const (
	// SignerURL is the signer base URL: required on both sides and validated
	// (http/https, non-empty host) wherever it is read.
	SignerURL = "SIGNER_URL"
	// SignerPublicKey is the pinned trusted public key: a literal
	// authorized_keys line or a path to a file containing one.
	SignerPublicKey = "SIGNER_PUBLIC_KEY"
	// SignerTimeout bounds one client HTTP round trip to the signer.
	SignerTimeout = "SIGNER_TIMEOUT"
)

// Installer and provisioning inputs consumed by deploy/install-server.sh and
// deploy/generate-key.sh. The deployment settings above are consumed under
// the same names (pure pass-through): the installer reads a setting by its
// runtime name and writes it into the env file verbatim.
const (
	SignerUser       = "SIGNER_USER"
	SignerGroup      = "SIGNER_GROUP"
	SignerKeyDir     = "SIGNER_KEY_DIR"
	SignerKeyName    = "SIGNER_KEY_NAME"
	SignerKeyComment = "SIGNER_KEY_COMMENT"
	SignerConfDir    = "SIGNER_CONF_DIR"
	SignerSkipChown  = "SIGNER_SKIP_CHOWN"
	SignerSkipDist   = "SIGNER_SKIP_DIST"
	SignerServerBin  = "SIGNER_SERVER_BIN"
	SignerRepoDir    = "SIGNER_REPO_DIR"
	SignerUnitFile   = "SIGNER_UNIT_FILE"
)

// DefaultSignerURL is the signer base URL used when SignerURL is unset: the
// canonical exe.dev peer-integration hostname. It is https, not http: the
// int.exe.xyz edge 301-redirects http to https, and a redirect in front of
// key fetches or the sign POST breaks clients that do not follow redirects
// (or follow them by downgrading POST to GET). TLS terminates at the edge;
// peer identity injection is unaffected.
const DefaultSignerURL = "https://git-signer.int.exe.xyz"
