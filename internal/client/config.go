package client

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/setting"
)

// Defaults applied when the corresponding environment variable is unset.
const (
	// DefaultTimeout bounds a single HTTP round trip to the signer.
	DefaultTimeout = 10 * time.Second
	// DefaultMaxResponseBytes caps the signature response body.
	DefaultMaxResponseBytes = 64 << 10 // 64 KiB
)

// config is the client configuration, sourced from the environment.
type config struct {
	// signURL is SIGNER_URL with any trailing slash removed.
	signURL string
	// pinned is the trusted public key.
	pinned publicKey
	// timeout is the HTTP client timeout.
	timeout time.Duration
	// maxResponseBytes caps the signature response body size.
	maxResponseBytes int64
}

// loadConfig reads and validates configuration through getenv.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		timeout:          DefaultTimeout,
		maxResponseBytes: DefaultMaxResponseBytes,
	}

	rawURL := strings.TrimSpace(getenv(setting.SignerURL))
	if rawURL == "" {
		return config{}, fmt.Errorf("%s is required", setting.SignerURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return config{}, fmt.Errorf("%s: %w", setting.SignerURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return config{}, fmt.Errorf("%s: unsupported scheme %q in %q", setting.SignerURL, u.Scheme, rawURL)
	}
	if u.Host == "" {
		return config{}, fmt.Errorf("%s: missing host in %q", setting.SignerURL, rawURL)
	}
	cfg.signURL = strings.TrimRight(rawURL, "/")

	pinned, err := loadPinnedKey(getenv(setting.SignerPublicKey))
	if err != nil {
		return config{}, err
	}
	cfg.pinned = pinned

	if raw := strings.TrimSpace(getenv(setting.SignerTimeout)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return config{}, fmt.Errorf("%s: %w", setting.SignerTimeout, err)
		}
		if d <= 0 {
			return config{}, fmt.Errorf("%s: timeout must be positive, got %q", setting.SignerTimeout, raw)
		}
		cfg.timeout = d
	}

	return cfg, nil
}

// publicKey is a parsed OpenSSH public key: its type and the base64 key blob.
// The trailing comment is deliberately ignored so that a pinned key and a
// user.signingkey file still match when their comments differ.
type publicKey struct {
	typ  string
	blob string
}

// loadPinnedKey resolves SIGNER_PUBLIC_KEY. The value may be either
// a literal authorized_keys line or a path to a readable file containing one.
func loadPinnedKey(value string) (publicKey, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return publicKey{}, fmt.Errorf("%s is required", setting.SignerPublicKey)
	}

	if pk, err := parsePublicKeyLine(v); err == nil {
		return pk, nil
	}

	data, err := os.ReadFile(v)
	if err != nil {
		return publicKey{}, fmt.Errorf("%s: not a valid SSH public key line and not a readable file: %w", setting.SignerPublicKey, err)
	}
	pk, err := parsePublicKeyLine(string(data))
	if err != nil {
		return publicKey{}, fmt.Errorf("%s: %s: %w", setting.SignerPublicKey, v, err)
	}
	return pk, nil
}

// readPublicKeyFile reads the key file named by Git's -f. The file's comment is
// ignored; only type and blob take part in the pinned-key comparison.
func readPublicKeyFile(path string) (publicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return publicKey{}, fmt.Errorf("read signing key %s: %w", path, err)
	}
	pk, err := parsePublicKeyLine(string(data))
	if err != nil {
		return publicKey{}, fmt.Errorf("signing key %s: %w", path, err)
	}
	return pk, nil
}

// parsePublicKeyLine parses an OpenSSH public key / authorized_keys line of the
// form "<type> <base64-blob> [comment]".
func parsePublicKeyLine(s string) (publicKey, error) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return publicKey{}, fmt.Errorf("not an SSH public key line")
	}
	typ := fields[0]
	if !isPublicKeyType(typ) {
		return publicKey{}, fmt.Errorf("unsupported public key type %q", typ)
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return publicKey{}, fmt.Errorf("invalid base64 key blob: %w", err)
	}
	return publicKey{typ: typ, blob: fields[1]}, nil
}

// isPublicKeyType reports whether typ names an OpenSSH public key algorithm.
func isPublicKeyType(typ string) bool {
	switch {
	case strings.HasPrefix(typ, "ssh-"),
		strings.HasPrefix(typ, "ecdsa-"),
		strings.HasPrefix(typ, "sk-"):
		return true
	default:
		return false
	}
}

// equal reports whether two public keys are the same key.
func (p publicKey) equal(other publicKey) bool {
	return p.typ == other.typ && p.blob == other.blob
}
