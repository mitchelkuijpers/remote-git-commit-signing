package server_test

import (
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/setting"
)

// mapEnv adapts a map to the getenv function shape LoadConfig expects.
func mapEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadConfigRequiresKeyPath(t *testing.T) {
	if _, err := server.LoadConfig(mapEnv(nil)); err == nil {
		t.Fatal("LoadConfig without key path = nil error, want error")
	}
	if _, err := server.LoadConfig(mapEnv(map[string]string{setting.SignerKeyPath: ""})); err == nil {
		t.Fatal("LoadConfig with empty key path = nil error, want error")
	}
}

func TestLoadConfigRequiresCommitterIdentity(t *testing.T) {
	base := map[string]string{
		setting.SignerKeyPath:        "/var/lib/git-signer/signing_key",
		setting.SignerCommitterName:  "Dev Eloper",
		setting.SignerCommitterEmail: "dev@example.com",
	}

	cases := []struct {
		name   string
		remove string
	}{
		{"name missing", setting.SignerCommitterName},
		{"email missing", setting.SignerCommitterEmail},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			delete(env, tc.remove)
			if _, err := server.LoadConfig(mapEnv(env)); err == nil {
				t.Fatalf("LoadConfig without %s = nil error, want error", tc.remove)
			}
		})
	}

	// Empty values are missing values, not a valid identity.
	empty := map[string]string{}
	for k, v := range base {
		empty[k] = v
	}
	empty[setting.SignerCommitterName] = ""
	if _, err := server.LoadConfig(mapEnv(empty)); err == nil {
		t.Fatal("LoadConfig with empty committer name = nil error, want error")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		setting.SignerKeyPath:        "/var/lib/git-signer/signing_key",
		setting.SignerCommitterName:  "Dev Eloper",
		setting.SignerCommitterEmail: "dev@example.com",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KeyPath != "/var/lib/git-signer/signing_key" {
		t.Fatalf("KeyPath = %q, want env value", cfg.KeyPath)
	}
	if cfg.Port != 8000 {
		t.Fatalf("Port = %d, want default 8000", cfg.Port)
	}
	if cfg.Handler.Committer.Name != "Dev Eloper" || cfg.Handler.Committer.Email != "dev@example.com" {
		t.Fatalf("committer identity = %q/%q, want parsed values", cfg.Handler.Committer.Name, cfg.Handler.Committer.Email)
	}
	// An unset SIGNER_URL falls back to the canonical peer-integration URL:
	// the bootstrap script and landing page render it, so there is no blank
	// default.
	if cfg.Handler.SignerURL != setting.DefaultSignerURL {
		t.Fatalf("SignerURL = %q, want default %s", cfg.Handler.SignerURL, setting.DefaultSignerURL)
	}
}

func TestLoadConfigReadsEnvironment(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		setting.SignerKeyPath:        "/keys/signing_key",
		setting.SignerPort:           "9100",
		setting.SignerCommitterName:  "Dev Eloper",
		setting.SignerCommitterEmail: "dev@example.com",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KeyPath != "/keys/signing_key" || cfg.Port != 9100 {
		t.Fatalf("config = %+v, want key path /keys/signing_key and port 9100", cfg)
	}
	if cfg.Handler.Committer.Name != "Dev Eloper" || cfg.Handler.Committer.Email != "dev@example.com" {
		t.Fatalf("committer identity = %q/%q, want parsed values", cfg.Handler.Committer.Name, cfg.Handler.Committer.Email)
	}
}

func TestLoadConfigRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"abc", "0", "-1", "70000"} {
		t.Run(port, func(t *testing.T) {
			if _, err := server.LoadConfig(mapEnv(map[string]string{
				setting.SignerKeyPath:        "/keys/signing_key",
				setting.SignerPort:           port,
				setting.SignerCommitterName:  "Dev Eloper",
				setting.SignerCommitterEmail: "dev@example.com",
			})); err == nil {
				t.Fatalf("LoadConfig with port %q = nil error, want error", port)
			}
		})
	}
}

func TestLoadConfigAllowlistDefaultsToNobody(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		setting.SignerKeyPath:        "/keys/signing_key",
		setting.SignerCommitterName:  "Dev Eloper",
		setting.SignerCommitterEmail: "dev@example.com",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Fail closed: an unset allowlist admits no VM identity.
	if len(cfg.Handler.Allowlist) != 0 {
		t.Fatalf("Allowlist = %v, want empty (refuse everyone)", cfg.Handler.Allowlist)
	}
	if cfg.Handler.RatePerMin != server.DefaultRatePerMin || cfg.Handler.RateBurst != server.DefaultRateBurst {
		t.Fatalf("rate limits = %d/%d, want defaults %d/%d",
			cfg.Handler.RatePerMin, cfg.Handler.RateBurst, server.DefaultRatePerMin, server.DefaultRateBurst)
	}
}

func TestLoadConfigParsesAllowlistAndRateLimits(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		setting.SignerKeyPath:        "/keys/signing_key",
		setting.SignerCommitterName:  "Dev Eloper",
		setting.SignerCommitterEmail: "dev@example.com",
		setting.SignerAllowlist:      " agent-a, agent-*, ,exact-vm ",
		setting.SignerRatePerMin:     "120",
		setting.SignerRateBurst:      "5",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := server.Allowlist{"agent-a", "agent-*", "exact-vm"}
	if len(cfg.Handler.Allowlist) != len(want) {
		t.Fatalf("Allowlist = %v, want %v", cfg.Handler.Allowlist, want)
	}
	for i := range want {
		if cfg.Handler.Allowlist[i] != want[i] {
			t.Fatalf("Allowlist = %v, want %v", cfg.Handler.Allowlist, want)
		}
	}
	if cfg.Handler.RatePerMin != 120 || cfg.Handler.RateBurst != 5 {
		t.Fatalf("rate limits = %d/%d, want 120/5", cfg.Handler.RatePerMin, cfg.Handler.RateBurst)
	}
}

func TestLoadConfigRejectsInvalidRateLimits(t *testing.T) {
	for _, env := range []string{setting.SignerRatePerMin, setting.SignerRateBurst} {
		for _, value := range []string{"abc", "0", "-1"} {
			t.Run(env+"="+value, func(t *testing.T) {
				if _, err := server.LoadConfig(mapEnv(map[string]string{
					setting.SignerKeyPath:        "/keys/signing_key",
					setting.SignerCommitterName:  "Dev Eloper",
					setting.SignerCommitterEmail: "dev@example.com",
					env:                          value,
				})); err == nil {
					t.Fatalf("LoadConfig with %s=%q = nil error, want error", env, value)
				}
			})
		}
	}
}

// TestLoadConfigValidatesSignerURL pins the SIGNER_URL contract: the value is
// interpolated into every client bootstrap, so a non-http(s) scheme or a
// scheme without a host must fail loading with an error naming the setting,
// and a trailing slash must be trimmed so templates that append endpoint
// paths never double one.
func TestLoadConfigValidatesSignerURL(t *testing.T) {
	base := map[string]string{
		setting.SignerKeyPath:        "/keys/signing_key",
		setting.SignerCommitterName:  "Dev Eloper",
		setting.SignerCommitterEmail: "dev@example.com",
	}
	envWith := func(url string) map[string]string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		env[setting.SignerURL] = url
		return env
	}

	for _, tc := range []struct {
		name string
		url  string
	}{
		{"scheme not http or https", "ftp://x"},
		{"missing host", "https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := server.LoadConfig(mapEnv(envWith(tc.url)))
			if err == nil {
				t.Fatalf("LoadConfig with %s=%q = nil error, want error", setting.SignerURL, tc.url)
			}
			if !strings.Contains(err.Error(), setting.SignerURL) {
				t.Fatalf("LoadConfig error %q does not name %s", err, setting.SignerURL)
			}
		})
	}

	t.Run("trailing slash trimmed", func(t *testing.T) {
		cfg, err := server.LoadConfig(mapEnv(envWith("http://x/")))
		if err != nil {
			t.Fatalf("LoadConfig with %s=http://x/: %v", setting.SignerURL, err)
		}
		if cfg.Handler.SignerURL != "http://x" {
			t.Fatalf("SignerURL = %q, want trailing slash trimmed to http://x", cfg.Handler.SignerURL)
		}
	})
}
