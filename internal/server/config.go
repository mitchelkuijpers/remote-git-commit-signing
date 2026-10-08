package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
)

// DefaultPort is the TCP port git-signer-server listens on when SIGNER_PORT is
// unset.
const DefaultPort = 8000

// Rate limit defaults for POST /v1/sign, applied per VM identity.
const (
	// DefaultRatePerMin is the sustained per-VM refill rate when
	// SIGNER_RATE_PER_MIN is unset.
	DefaultRatePerMin = 60
	// DefaultRateBurst is the per-VM token bucket capacity when
	// SIGNER_RATE_BURST is unset.
	DefaultRateBurst = 10
)

// Environment variable names understood by LoadConfig.
const (
	envKeyPath        = "SIGNER_KEY_PATH"
	envPort           = "SIGNER_PORT"
	envCommitterName  = "SIGNER_COMMITTER_NAME"
	envCommitterEmail = "SIGNER_COMMITTER_EMAIL"
	envAllowlist      = "SIGNER_ALLOWLIST"
	envRatePerMin     = "SIGNER_RATE_PER_MIN"
	envRateBurst      = "SIGNER_RATE_BURST"
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
	// Allowlist is the set of VM identities permitted to sign, given as exact
	// names or path.Match glob patterns. There is no allow-all default: when
	// SIGNER_ALLOWLIST is unset the list is empty and every request is refused.
	Allowlist Allowlist
	// RatePerMin is the sustained per-VM refill rate for signing requests.
	// Defaults to DefaultRatePerMin.
	RatePerMin int
	// RateBurst is the per-VM token bucket capacity, i.e. the largest burst of
	// signing requests admitted at once. Defaults to DefaultRateBurst.
	RateBurst int
}

// LoadConfig reads configuration through getenv (normally os.Getenv). It
// returns an error for a missing key path or an unparsable port.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		KeyPath:        getenv(envKeyPath),
		Port:           DefaultPort,
		CommitterName:  getenv(envCommitterName),
		CommitterEmail: getenv(envCommitterEmail),
		RatePerMin:     DefaultRatePerMin,
		RateBurst:      DefaultRateBurst,
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

	cfg.Allowlist = parseAllowlist(getenv(envAllowlist))

	if raw := getenv(envRatePerMin); raw != "" {
		rate, err := parsePositiveInt(envRatePerMin, raw)
		if err != nil {
			return Config{}, err
		}
		cfg.RatePerMin = rate
	}

	if raw := getenv(envRateBurst); raw != "" {
		burst, err := parsePositiveInt(envRateBurst, raw)
		if err != nil {
			return Config{}, err
		}
		cfg.RateBurst = burst
	}

	return cfg, nil
}

// parseAllowlist splits a comma-separated allowlist, trimming blank entries.
// An empty or unset value yields an empty allowlist: refuse everyone.
func parseAllowlist(raw string) Allowlist {
	var list Allowlist
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			list = append(list, entry)
		}
	}
	return list
}

// parsePositiveInt parses a strictly positive integer setting.
func parsePositiveInt(name, raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s: invalid value %q", name, raw)
	}
	return n, nil
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
	if c.RatePerMin <= 0 {
		c.RatePerMin = DefaultRatePerMin
	}
	if c.RateBurst <= 0 {
		c.RateBurst = DefaultRateBurst
	}
	return c
}
