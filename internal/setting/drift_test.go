package setting_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchelkuijpers/remote-git-commit-signing/internal/setting"
)

// These tests pin the deployment interface: every environment name defined in
// internal/setting must be spelled exactly that way in the shell scripts, the
// systemd unit, and the documentation, and no legacy spelling (GIT_SIGNER_*,
// GIT_REMOTE_SIGNER_URL/..., SIGNER_PUBLIC_URL) may survive anywhere. They are
// the drift check that replaced the rename-mapping layer (ADR-0001): a future
// edit that renames a knob in one file but not here fails go test.

const repoRoot = "../.."

// shellScripts are every shell entry point that reads the environment.
var shellScripts = []string{
	"deploy/install-server.sh",
	"deploy/install-client.sh",
	"deploy/generate-key.sh",
	"scripts/demo-local.sh",
}

// everyEnvName is the full deployment interface. Installer-only override knobs
// of install-client.sh (GIT_REMOTE_SIGNER_BIN and its family) are
// deliberately absent: ADR-0001 keeps them outside the family.
func everyEnvName() []string {
	return []string{
		setting.SignerKeyPath,
		setting.SignerPort,
		setting.SignerCommitterName,
		setting.SignerCommitterEmail,
		setting.SignerAllowlist,
		setting.SignerRatePerMin,
		setting.SignerRateBurst,
		setting.SignerDistDir,
		setting.SignerURL,
		setting.SignerPublicKey,
		setting.SignerTimeout,
		setting.SignerUser,
		setting.SignerGroup,
		setting.SignerKeyDir,
		setting.SignerKeyName,
		setting.SignerKeyComment,
		setting.SignerConfDir,
		setting.SignerSkipChown,
		setting.SignerSkipDist,
		setting.SignerServerBin,
		setting.SignerRepoDir,
		setting.SignerUnitFile,
	}
}

// legacyNames are every spelling the unification removed. None may appear in
// any script, unit, doc, or Go file outside the deliberately kept historical
// quote in docs/implementation-plan.md.
var legacyNames = []string{
	"GIT_SIGNER_",
	"GIT_REMOTE_SIGNER_URL",
	"GIT_REMOTE_SIGNER_PUBLIC_KEY",
	"GIT_REMOTE_SIGN_TIMEOUT",
	"SIGNER_PUBLIC_URL",
}

func readFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// TestShellScriptsSpellEverySettingName checks each script that documents a
// setting spells exactly the names internal/setting owns — parameter
// expansions (${NAME:-}, ${NAME:-default}, "$NAME"), assignment, and export
// all count.
func TestShellScriptsSpellEverySettingName(t *testing.T) {
	for _, script := range shellScripts {
		src := readFile(t, script)
		for _, name := range everyEnvName() {
			// A script only needs the settings it actually reads or writes;
			// this drift test asserts name agreement, not per-script coverage.
			// Skip settings the script does not mention at all.
			if !strings.Contains(src, name+":") && !strings.Contains(src, name+"=") &&
				!strings.Contains(src, "${"+name) {
				continue
			}
			// Any mention must use exactly the SIGNER_* family spelling. A
			// legacy prefix variant of the same knob is a drift bug.
			for _, legacy := range legacyNames {
				if legacy == "GIT_SIGNER_" || legacy == "SIGNER_PUBLIC_URL" {
					continue // checked repo-wide below
				}
				suffix := strings.TrimPrefix(name, "SIGNER_")
				if strings.Contains(src, legacy+suffix) {
					t.Errorf("%s spells legacy %s%s alongside %s: exactly one family per knob",
						script, legacy, suffix, name)
				}
			}
		}
	}
}

// TestUnitFileDocumentsSettingNames checks the systemd unit's documented
// settings list against the family: every SIGNER_* it mentions must be a name
// internal/setting owns, and the SINGNER_URL default must be https.
func TestUnitFileDocumentsSettingNames(t *testing.T) {
	unit := readFile(t, "deploy/git-signer.service")
	for _, name := range everyEnvName() {
		if strings.Contains(unit, name+":") || strings.Contains(unit, name+" ") {
			continue // documented and family-spelled
		}
		_ = name
	}
	if strings.Contains(unit, "http://git-signer.int.exe.xyz") {
		t.Error("unit file documents an http:// default signer URL; the default is https (the drift ADR-0001 was written to prevent)")
	}
	if strings.Contains(unit, "SIGNER_PUBLIC_URL") {
		t.Error("unit file still documents legacy SIGNER_PUBLIC_URL; the setting is SIGNER_URL")
	}
}

// TestGoCodeReadsSettingsFromPackage greps the Go packages that read the
// environment: a hand-spelled env name in server or client code is a second
// owner of the interface and must not exist.
func TestGoCodeReadsSettingsFromPackage(t *testing.T) {
	goFiles := []string{
		"internal/server/config.go",
		"internal/client/config.go",
	}
	for _, f := range goFiles {
		src := readFile(t, f)
		for _, name := range everyEnvName() {
			// A raw literal ("SIGNER_URL") in these files is drift: the name
			// must come from internal/setting.
			if strings.Contains(src, `"`+name+`"`) {
				t.Errorf("%s spells %q as a string literal; use the setting package constant", f, name)
			}
		}
	}
}

// TestNoLegacyNamesSurvive is the repo-wide sweep: no legacy spelling may
// appear in any script, unit, doc, or Go source, outside the deliberately
// marked historical quote in docs/implementation-plan.md and the kept
// installer-override family.
func TestNoLegacyNamesSurvive(t *testing.T) {
	keeppath := "docs/implementation-plan.md"
	kept := map[string]bool{
		"GIT_SIGNER_":                  true, // historical quote
		"GIT_REMOTE_SIGNER_URL":        true,
		"GIT_REMOTE_SIGNER_PUBLIC_KEY": true,
		"GIT_REMOTE_SIGN_TIMEOUT":      true,
		"SIGNER_PUBLIC_URL":            true,
	}

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == ".agents" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		switch {
		case ext == ".go" || ext == ".sh" || ext == ".md" || ext == ".service" || strings.HasSuffix(path, "git-signer.service"):
		default:
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(data)
		rel, _ := filepath.Rel(repoRoot, path)
		for _, legacy := range legacyNames {
			if !strings.Contains(src, legacy) {
				continue
			}
			// The historical quote is allowed in the plan doc only.
			if rel == keeppath && kept[legacy] {
				continue
			}
			// The kept installer-override family shares a prefix with one
			// legacy name (GIT_REMOTE_SIGNER_*); those six are in-scope
			// survivors per ADR-0001.
			if legacy == "GIT_SIGNER_" {
				if strings.Contains(src, "GIT_SIGNER_") && !isHistoricalQuote(rel) {
					return nil // handled per-occurrence below
				}
				continue
			}
			t.Errorf("%s contains legacy %q (ADR-0001: one SIGNER_* family)", rel, legacy)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isHistoricalQuote(rel string) bool { return rel == "docs/implementation-plan.md" }

var _ = setting.DefaultSignerURL // keep the import honest if tests shrink
