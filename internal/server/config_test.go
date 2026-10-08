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

func TestLoadConfigRequiresCommitterIdentity(t *testing.T) {
	base := map[string]string{
		"SIGNER_KEY_PATH":        "/var/lib/git-signer/signing_key",
		"SIGNER_COMMITTER_NAME":  "Dev Eloper",
		"SIGNER_COMMITTER_EMAIL": "dev@example.com",
	}

	cases := []struct {
		name   string
		remove string
	}{
		{"name missing", "SIGNER_COMMITTER_NAME"},
		{"email missing", "SIGNER_COMMITTER_EMAIL"},
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
	empty["SIGNER_COMMITTER_NAME"] = ""
	if _, err := server.LoadConfig(mapEnv(empty)); err == nil {
		t.Fatal("LoadConfig with empty committer name = nil error, want error")
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := server.LoadConfig(mapEnv(map[string]string{
		"SIGNER_KEY_PATH":        "/var/lib/git-signer/signing_key",
		"SIGNER_COMMITTER_NAME":  "Dev Eloper",
		"SIGNER_COMMITTER_EMAIL": "dev@example.com",
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
	if cfg.CommitterName != "Dev Eloper" || cfg.CommitterEmail != "dev@example.com" {
		t.Fatalf("committer identity = %q/%q, want parsed values", cfg.CommitterName, cfg.CommitterEmail)
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
				"SIGNER_KEY_PATH":        "/keys/signing_key",
				"SIGNER_PORT":            port,
				"SIGNER_COMMITTER_NAME":  "Dev Eloper",
				"SIGNER_COMMITTER_EMAIL": "dev@example.com",
			})); err == nil {
				t.Fatalf("LoadConfig with port %q = nil error, want error", port)
			}
		})
	}
}
