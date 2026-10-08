package server

import (
	"fmt"
	"strconv"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// DefaultPort is the TCP port git-signer-server listens on when SIGNER_PORT is
// unset.
const DefaultPort = 8000

// Environment variable names understood by LoadConfig.
const (
	envKeyPath        = "SIGNER_KEY_PATH"
	envPort           = "SIGNER_PORT"
	envCommitterName  = "SIGNER_COMMITTER_NAME"
	envCommitterEmail = "SIGNER_COMMITTER_EMAIL"
)

// Config is the server configuration, sourced from the environment.
type Config struct {
	// KeyPath is the path to the private signing key. Required: an empty value
	// is a configuration error, there is no default.
	KeyPath string
	// Port is the TCP port to listen on. Defaults to DefaultPort.
	Port int
	// MaxPayloadBytes caps the request body accepted by POST /v1/sign.
	// Defaults to signing.DefaultMaxPayloadBytes.
	MaxPayloadBytes int64
	// SignTimeout bounds a single signing request. Defaults to
	// signing.DefaultTimeout.
	SignTimeout time.Duration
	// CommitterName and CommitterEmail are the pinned committer identity.
	// Both are required with no default: POST /v1/sign only signs commit
	// objects naming exactly this identity.
	CommitterName  string
	CommitterEmail string
}

// LoadConfig reads configuration through getenv (normally os.Getenv). It
// returns an error for a missing key path or an unparsable port.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		KeyPath:        getenv(envKeyPath),
		Port:           DefaultPort,
		CommitterName:  getenv(envCommitterName),
		CommitterEmail: getenv(envCommitterEmail),
	}

	if cfg.KeyPath == "" {
		return Config{}, fmt.Errorf("%s is required", envKeyPath)
	}

	// The pinned committer identity is required with no default: the signer
	// refuses to sign a commit that names anything else, so a deployment
	// without it could never sign anything.
	if cfg.CommitterName == "" {
		return Config{}, fmt.Errorf("%s is required", envCommitterName)
	}
	if cfg.CommitterEmail == "" {
		return Config{}, fmt.Errorf("%s is required", envCommitterEmail)
	}

	if raw := getenv(envPort); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("%s: invalid port %q", envPort, raw)
		}
		cfg.Port = port
	}

	return cfg, nil
}

// withDefaults fills the optional fields and returns the result.
func (c Config) withDefaults() Config {
	if c.Port <= 0 {
		c.Port = DefaultPort
	}
	if c.MaxPayloadBytes <= 0 {
		c.MaxPayloadBytes = signing.DefaultMaxPayloadBytes
	}
	if c.SignTimeout <= 0 {
		c.SignTimeout = signing.DefaultTimeout
	}
	return c
}
