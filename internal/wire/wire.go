// Package wire is the single source of the HTTP wire contract shared by
// git-signer-server and git-remote-sign: endpoint paths, media types, the
// payload cap, and the trust header.
//
// Both binaries and the client's test mock consume these constants, so the
// contract is spelled once: the server's handlers, the client's signing
// request, and the installer's endpoint curls all derive from here. Contract
// tests elsewhere may spell the values again deliberately, as independent
// pins against this definition drifting.
package wire

// SourceVMHeader is the HTTP header carrying the platform-verified source-VM
// identity. The exe.dev authenticated peer proxy sets it in production; a
// client cannot forge it through the public route. devproxy and the test
// harnesses stamp it to simulate the platform.
const SourceVMHeader = "X-Exedev-Source-Vm"

// Endpoint paths served by git-signer-server.
const (
	// SignPath signs a commit payload (identity-gated).
	SignPath = "/v1/sign"
	// PublicKeyPath serves the public signing key (informational; clients pin).
	PublicKeyPath = "/v1/public-key"
	// HealthzPath is the liveness probe.
	HealthzPath = "/healthz"
	// ReadyzPath is the readiness probe (503 until the key is loaded).
	ReadyzPath = "/readyz"
	// InstallScriptPath renders the client bootstrap script.
	InstallScriptPath = "/install.sh"
	// ClientFilesPath is the prefix under which whitelisted client-distribution
	// files are served; the download name completes it.
	ClientFilesPath = "/v1/client/"
)

// Media types of the signing request and response.
const (
	// ContentTypeOctetStream is the media type of a raw payload
	// (signing request body; client-distribution binaries).
	ContentTypeOctetStream = "application/octet-stream"
	// ContentTypeSSHSig is the media type of a raw SSHSIG signature.
	ContentTypeSSHSig = "application/vnd.sshsig"
)

// MaxPayloadBytes caps the commit payload accepted by POST /v1/sign and by the
// signing backend. The client relies on the 413 the server returns for larger
// payloads.
const MaxPayloadBytes int64 = 1 << 20 // 1 MiB
