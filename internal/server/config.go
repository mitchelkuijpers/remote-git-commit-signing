package server

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/setting"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/signing"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/wire"
)

// DefaultPort is the TCP port git-signer-server listens on when
// setting.SignerPort is unset.
const DefaultPort = 8000

// Rate limit defaults for POST /v1/sign, applied per VM identity.
const (
	// DefaultRatePerMin is the sustained per-VM refill rate when
	// setting.SignerRatePerMin is unset.
	DefaultRatePerMin = 60
	// DefaultRateBurst is the per-VM token bucket capacity when
	// setting.SignerRateBurst is unset.
	DefaultRateBurst = 10
)

// Committer is the pinned Git committer identity a commit must name to be
// signed. Both fields are required with no default.
type Committer struct {
	Name  string
	Email string
}

// Matches reports whether name and email identify exactly this committer.
func (c Committer) Matches(name, email string) bool {
	return c.Name == name && c.Email == email
}

// Config is the server process configuration, sourced from the environment.
type Config struct {
	// KeyPath is the path to the private signing key. Required: an empty value
	// is a configuration error, there is no default.
	KeyPath string
	// Port is the TCP port to listen on. Defaults to DefaultPort.
	Port int
	// Handler carries the request-facing configuration consumed by New: see
	// HandlerConfig for the fields and their defaults. Defaults are applied
	// by WithDefaults.
	Handler HandlerConfig
}

// HandlerConfig is the request-facing configuration of the HTTP handler set.
// It is the seam between the process (Config, which loads it) and the handler
// (New, which consumes it): everything the signing endpoints need except the
// signer, the public key, and the logger.
type HandlerConfig struct {
	// Committer is the pinned committer identity. Both fields are required
	// with no default: POST /v1/sign only signs commit objects naming exactly
	// this identity.
	Committer Committer
	// Allowlist is the set of VM identities permitted to sign, given as exact
	// names or path.Match glob patterns. There is no allow-all default: when
	// setting.SignerAllowlist is unset the list is empty and every request is
	// refused.
	Allowlist Allowlist
	// RatePerMin is the sustained per-VM refill rate for signing requests.
	// Defaults to DefaultRatePerMin.
	RatePerMin int
	// RateBurst is the per-VM token bucket capacity, i.e. the largest burst of
	// signing requests admitted at once. Defaults to DefaultRateBurst.
	RateBurst int
	// DistDir is the directory the client-distribution endpoints serve
	// (git-remote-sign builds and install-client.sh). Empty disables those
	// endpoints: GET /v1/client/... answers 404. GET /install.sh is served
	// regardless.
	DistDir string
	// SignerURL is the signer base URL rendered into the client bootstrap
	// script (GET /install.sh) and shown on the landing page. It must be the
	// exe.dev peer-integration URL, not a direct VM URL: only the peer
	// integration delivers the verified caller identity POST /v1/sign needs.
	// LoadConfig validates it; withDefaults fills setting.DefaultSignerURL.
	SignerURL string
	// MaxPayloadBytes caps the request body accepted by POST /v1/sign.
	// Defaults to wire.MaxPayloadBytes.
	MaxPayloadBytes int64
	// SignTimeout bounds a single signing request. Defaults to
	// signing.DefaultKeygenTimeout via Config.Handler; New does not apply
	// defaults itself.
	SignTimeout time.Duration
}

// WithDefaults returns the handler configuration with defaults filled: the
// environment-facing Config is the source, the handler-facing HandlerConfig
// the result New consumes.
func (c Config) WithDefaults() HandlerConfig {
	return c.Handler.withDefaults()
}

// LoadConfig reads configuration through getenv (normally os.Getenv). It
// returns an error for a missing key path, an unparsable port, or an invalid
// setting.SignerURL.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		KeyPath: getenv(setting.SignerKeyPath),
		Port:    DefaultPort,
		Handler: HandlerConfig{
			Committer: Committer{
				Name:  getenv(setting.SignerCommitterName),
				Email: getenv(setting.SignerCommitterEmail),
			},
			RatePerMin: DefaultRatePerMin,
			RateBurst:  DefaultRateBurst,
			DistDir:    getenv(setting.SignerDistDir),
			SignerURL:  setting.DefaultSignerURL,
		},
	}

	if cfg.KeyPath == "" {
		return Config{}, fmt.Errorf("%s is required", setting.SignerKeyPath)
	}

	// The pinned committer identity is required with no default: the signer
	// refuses to sign a commit that names anything else, so a deployment
	// without it could never sign anything.
	if cfg.Handler.Committer.Name == "" {
		return Config{}, fmt.Errorf("%s is required", setting.SignerCommitterName)
	}
	if cfg.Handler.Committer.Email == "" {
		return Config{}, fmt.Errorf("%s is required", setting.SignerCommitterEmail)
	}

	if raw := getenv(setting.SignerPort); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("%s: invalid port %q", setting.SignerPort, raw)
		}
		cfg.Port = port
	}

	cfg.Handler.Allowlist = parseAllowlist(getenv(setting.SignerAllowlist))

	signerURL, err := parseSignerURL(getenv(setting.SignerURL), setting.DefaultSignerURL)
	if err != nil {
		return Config{}, err
	}
	cfg.Handler.SignerURL = signerURL

	if raw := getenv(setting.SignerRatePerMin); raw != "" {
		rate, err := parsePositiveInt(setting.SignerRatePerMin, raw)
		if err != nil {
			return Config{}, err
		}
		cfg.Handler.RatePerMin = rate
	}

	if raw := getenv(setting.SignerRateBurst); raw != "" {
		burst, err := parsePositiveInt(setting.SignerRateBurst, raw)
		if err != nil {
			return Config{}, err
		}
		cfg.Handler.RateBurst = burst
	}

	return cfg, nil
}

// parseSignerURL validates the signer base URL read from setting.SignerURL:
// it must parse as an absolute URL with an http or https scheme and name a
// host — the bootstrap script interpolates it, and a malformed value would
// break every client install, not just this server. Empty falls back to
// fallback. The result carries no trailing slash, so the templates that append
// endpoint paths never double one.
func parseSignerURL(raw, fallback string) (string, error) {
	if raw == "" {
		return strings.TrimRight(fallback, "/"), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s: invalid URL %q", setting.SignerURL, raw)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("%s: must be an absolute http(s) URL with a host, got %q", setting.SignerURL, raw)
	}
	return strings.TrimRight(raw, "/"), nil
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
func (h HandlerConfig) withDefaults() HandlerConfig {
	if h.SignerURL == "" {
		h.SignerURL = setting.DefaultSignerURL
	}
	h.SignerURL = strings.TrimRight(h.SignerURL, "/")
	if h.MaxPayloadBytes <= 0 {
		h.MaxPayloadBytes = wire.MaxPayloadBytes
	}
	if h.SignTimeout <= 0 {
		h.SignTimeout = signing.DefaultKeygenTimeout
	}
	if h.RatePerMin <= 0 {
		h.RatePerMin = DefaultRatePerMin
	}
	if h.RateBurst <= 0 {
		h.RateBurst = DefaultRateBurst
	}
	return h
}
