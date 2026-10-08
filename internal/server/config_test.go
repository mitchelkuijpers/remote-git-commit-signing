package server_test

import (
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/server"
)

// mapEnv adapts a map to the getenv function shape LoadConfig expects.
func mapEnv(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadConfigRequiresKeyPath(t *testing.T) {
	if _, err := server.LoadConfig(mapEnv(nil)); err == nil {
		t.Fatal("LoadConfig without key path = nil error, want error")
	}
	if _, err := server.LoadConfig(mapEnv(map[string]string{"SIGNER_KEY_PATH": ""})); err == nil {
		t.Fatal("LoadConfig with empty key path = nil error, want error")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{"SIGNER_KEY_PATH": "/var/lib/git-signer/signing_key"}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KeyPath != "/var/lib/git-signer/signing_key" {
		t.Fatalf("KeyPath = %q, want env value", cfg.KeyPath)
	}
	if cfg.Port != 8000 {
		t.Fatalf("Port = %d, want default 8000", cfg.Port)
	}
	// Committer identity is parsed but not enforced until ticket #6.
	if cfg.CommitterName != "" || cfg.CommitterEmail != "" {
		t.Fatalf("committer identity = %q/%q, want empty", cfg.CommitterName, cfg.CommitterEmail)
	}
}

func TestLoadConfigReadsEnvironment(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		"SIGNER_KEY_PATH":        "/keys/signing_key",
		"SIGNER_PORT":            "9100",
		"SIGNER_COMMITTER_NAME":  "Dev Eloper",
		"SIGNER_COMMITTER_EMAIL": "dev@example.com",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.KeyPath != "/keys/signing_key" || cfg.Port != 9100 {
		t.Fatalf("config = %+v, want key path /keys/signing_key and port 9100", cfg)
	}
	if cfg.CommitterName != "Dev Eloper" || cfg.CommitterEmail != "dev@example.com" {
		t.Fatalf("committer identity = %q/%q, want parsed values", cfg.CommitterName, cfg.CommitterEmail)
	}
}

func TestLoadConfigRejectsInvalidPort(t *testing.T) {
	for _, port := range []string{"abc", "0", "-1", "70000"} {
		t.Run(port, func(t *testing.T) {
			if _, err := server.LoadConfig(mapEnv(map[string]string{
				"SIGNER_KEY_PATH": "/keys/signing_key",
				"SIGNER_PORT":     port,
			})); err == nil {
				t.Fatalf("LoadConfig with port %q = nil error, want error", port)
			}
		})
	}
}

func TestLoadConfigAllowlistDefaultsToNobody(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{"SIGNER_KEY_PATH": "/keys/signing_key"}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Fail closed: an unset allowlist admits no VM identity.
	if len(cfg.Allowlist) != 0 {
		t.Fatalf("Allowlist = %v, want empty (refuse everyone)", cfg.Allowlist)
	}
	if cfg.RatePerMin != server.DefaultRatePerMin || cfg.RateBurst != server.DefaultRateBurst {
		t.Fatalf("rate limits = %d/%d, want defaults %d/%d",
			cfg.RatePerMin, cfg.RateBurst, server.DefaultRatePerMin, server.DefaultRateBurst)
	}
}

func TestLoadConfigParsesAllowlistAndRateLimits(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		"SIGNER_KEY_PATH":     "/keys/signing_key",
		"SIGNER_ALLOWLIST":    " agent-a, agent-*, ,exact-vm ",
		"SIGNER_RATE_PER_MIN": "120",
		"SIGNER_RATE_BURST":   "5",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := server.Allowlist{"agent-a", "agent-*", "exact-vm"}
	if len(cfg.Allowlist) != len(want) {
		t.Fatalf("Allowlist = %v, want %v", cfg.Allowlist, want)
	}
	for i := range want {
		if cfg.Allowlist[i] != want[i] {
			t.Fatalf("Allowlist = %v, want %v", cfg.Allowlist, want)
		}
	}
	if cfg.RatePerMin != 120 || cfg.RateBurst != 5 {
		t.Fatalf("rate limits = %d/%d, want 120/5", cfg.RatePerMin, cfg.RateBurst)
	}
}

func TestLoadConfigRejectsInvalidRateLimits(t *testing.T) {
	for _, env := range []string{"SIGNER_RATE_PER_MIN", "SIGNER_RATE_BURST"} {
		for _, value := range []string{"abc", "0", "-1"} {
			t.Run(env+"="+value, func(t *testing.T) {
				if _, err := server.LoadConfig(mapEnv(map[string]string{
					"SIGNER_KEY_PATH": "/keys/signing_key",
					env:               value,
				})); err == nil {
					t.Fatalf("LoadConfig with %s=%q = nil error, want error", env, value)
				}
			})
		}
	}
}
