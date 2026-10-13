package scripts_test

// Tests for scripts/build-release.sh, the canonical release-artifact recipe.
//
// The seam is the artifact contract its consumers depend on:
// deploy/install-client.sh looks up the client tarball by the exact name
// `git-remote-sign_<version>_<os>_<arch>.tar.gz` in checksums.txt
// (`awk -v a="$artifact" '$2 == a'`), the signer's client bootstrap downloads
// the raw `git-remote-sign-linux-<arch>` binaries, and each tarball must
// contain a `git-remote-sign` binary the installer can find. A name, format,
// or content drift here breaks the one-liner provisioning path, so this test
// pins the whole contract end-to-end by running the script for real.

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const buildScriptPath = "scripts/build-release.sh"

// expectedAssets is the full artifact set the release workflow attaches, for
// version 0.2.0: client tarballs for every platform install-client.sh
// advertises (linux and darwin, amd64 and arm64), raw client binaries (the
// signer's dist directory serves the linux ones), linux server binaries, and
// the checksum manifest.
var expectedAssets = []string{
	"git-remote-sign_0.2.0_linux_amd64.tar.gz",
	"git-remote-sign_0.2.0_linux_arm64.tar.gz",
	"git-remote-sign_0.2.0_darwin_amd64.tar.gz",
	"git-remote-sign_0.2.0_darwin_arm64.tar.gz",
	"git-remote-sign-linux-amd64",
	"git-remote-sign-linux-arm64",
	"git-remote-sign-darwin-amd64",
	"git-remote-sign-darwin-arm64",
	"git-signer-server-linux-amd64",
	"git-signer-server-linux-arm64",
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// runBuildRelease executes the script for the given version into a temporary
// output directory and returns that directory's path.
func runBuildRelease(t *testing.T, version string) string {
	t.Helper()
	root := moduleRoot(t)
	out := t.TempDir()

	cmd := exec.Command("/bin/sh", filepath.Join(root, buildScriptPath), version, out)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run %s %s: %v", buildScriptPath, version, err)
	}
	return out
}

func TestBuildReleaseProducesArtifactContract(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the full artifact set")
	}

	// The leading `v` must be accepted: the workflow passes the tag name.
	out := runBuildRelease(t, "v0.2.0")
	dir := filepath.Join(out, "v0.2.0")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v (did the script create v<version>/?)", dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := append([]string{"checksums.txt"}, expectedAssets...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("artifact set mismatch.\ngot:  %v\nwant: %v", got, want)
	}

	// Raw binaries must be executable; tarballs just regular files (the
	// binary inside carries its own mode, asserted separately below).
	for _, name := range expectedAssets {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file (mode %s)", name, info.Mode())
		}
		if !strings.HasSuffix(name, ".tar.gz") && info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %s)", name, info.Mode())
		}
	}
}

func TestBuildReleaseChecksumsManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the full artifact set")
	}

	dir := filepath.Join(runBuildRelease(t, "v0.2.0"), "v0.2.0")
	data, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatalf("read checksums.txt: %v", err)
	}

	// sha256sum format, exactly as install-client.sh's awk filter expects:
	// "<64 hex>  <filename>", two spaces, one line per asset, nothing else.
	lineRe := regexp.MustCompile(`^[0-9a-f]{64}  (\S+)$`)
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(expectedAssets) {
		t.Fatalf("checksums.txt has %d lines, want %d:\n%s", len(lines), len(expectedAssets), data)
	}
	got := make(map[string]string, len(lines))
	for _, line := range lines {
		m := lineRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("checksums.txt line is not sha256sum format: %q", line)
		}
		got[m[1]] = "ok"
	}
	for _, name := range expectedAssets {
		if _, ok := got[name]; !ok {
			t.Fatalf("checksums.txt has no entry for %q (install-client.sh would refuse the install): %v", name, got)
		}
	}
	if len(got) != len(expectedAssets) {
		t.Fatalf("checksums.txt covers %d assets, want exactly %d: %v", len(got), len(expectedAssets), got)
	}

	// The manifest must actually verify: `sha256sum -c` in the directory.
	cmd := exec.Command("sha256sum", "-c", "checksums.txt")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sha256sum -c failed: %v\n%s", err, out)
	}
}

func TestBuildReleaseTarballsContainClientBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the full artifact set")
	}

	dir := filepath.Join(runBuildRelease(t, "v0.2.0"), "v0.2.0")
	tarballs, err := filepath.Glob(filepath.Join(dir, "git-remote-sign_0.2.0_*.tar.gz"))
	if err != nil || len(tarballs) != 4 {
		t.Fatalf("expected 4 client tarballs, got %v (err %v)", tarballs, err)
	}
	for _, path := range tarballs {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("%s is not gzip: %v", path, err)
		}
		tr := tar.NewReader(gz)
		found := false
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if hdr.Name == "git-remote-sign" && hdr.Typeflag == tar.TypeReg && hdr.Mode&0o111 != 0 {
				found = true
			}
		}
		f.Close()
		if !found {
			t.Fatalf("%s does not contain an executable regular file named git-remote-sign", path)
		}
	}
}

func TestBuildReleaseHostBinaryRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles the full artifact set")
	}
	if runtime.GOOS != "linux" {
		t.Skip("host-run check needs a linux host")
	}

	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		t.Skipf("no release artifact for host arch %s", arch)
	}

	dir := filepath.Join(runBuildRelease(t, "v0.2.0"), "v0.2.0")
	bin := filepath.Join(dir, "git-remote-sign-linux-"+arch)
	// No args is a usage failure, but a *clean* one: exit non-zero with a
	// message on stderr proves the binary is a working executable, not a
	// stub or a mis-architected build.
	out, err := exec.Command(bin).CombinedOutput()
	if err == nil {
		t.Fatalf("git-remote-sign with no args unexpectedly succeeded: %s", out)
	}
	if len(out) == 0 {
		t.Fatal("git-remote-sign with no args printed nothing; not a working executable")
	}
}
