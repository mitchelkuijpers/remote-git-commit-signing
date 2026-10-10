#!/bin/sh
#
# install-client.sh -- provision a fresh exe.dev agent VM for remote commit
# signing in one idempotent run.
#
# What it does, in order:
#   1. validates configuration and the tools it needs (POSIX sh; no Nix,
#      Docker, or other heavyweight runtime)
#   2. checks the signer is reachable (GET /healthz)
#   3. cross-checks the pinned public key against GET /v1/public-key and fails
#      hard on mismatch
#   4. installs the git-remote-sign binary: from a local path
#      (GIT_REMOTE_SIGNER_BIN) or from a checksum-verified release artifact
#   5. installs the pinned public key to <config>/signing.pub and a matching
#      allowed-signers file to <config>/allowed_signers (so local
#      `git verify-commit` trusts the pinned key with no manual setup)
#   6. writes the user-level git config (ssh format, signer program,
#      auto-signing, signingkey, allowed-signers file, developer name/email)
#      and touches no other git setting
#   7. persists the two client environment variables in <config>/env and
#      sources them from the login profile
#   8. runs a harmless signing self-test in a throwaway repository that is
#      removed afterwards (never pushes anywhere)
#
# Re-running is safe: every step converges to the same state. Nothing private
# is ever written to the VM -- there is no private key, token, or signing
# credential in any file this script creates.
#
# Usage: deploy/install-client.sh [--skip-selftest]
#
# Required environment:
#   SIGNER_URL          signer base URL. In production this is the
#                                  exe.dev peer-integration URL; the platform
#                                  proxy stamps the verified source-VM identity
#                                  that POST /v1/sign requires.
#   SIGNER_PUBLIC_KEY   pinned public key: a literal authorized_keys
#                                  line, or a path to a file containing one.
#   SIGNER_COMMITTER_NAME      developer name written to git user.name
#   SIGNER_COMMITTER_EMAIL     developer email written to git user.email
#
# Optional environment:
#   GIT_REMOTE_SIGNER_BIN          local git-remote-sign binary to install
#                                  (skips the download)
#   GIT_REMOTE_SIGNER_RELEASE_VERSION  release tag (default: v0.1.0)
#   GIT_REMOTE_SIGNER_DOWNLOAD_BASE    release download base URL
#                                  (default: the project's GitHub releases)
#   GIT_REMOTE_SIGNER_INSTALL_DIR  binary directory (default: $HOME/.local/bin)
#   GIT_REMOTE_SIGNER_CONFIG_DIR   config directory (default:
#                                  $XDG_CONFIG_HOME/git-remote-signer)
#   GIT_REMOTE_SIGNER_PROFILE      login profile that sources <config>/env
#                                  (default: $HOME/.profile)
#   SIGNER_TIMEOUT        optional client timeout persisted to env
#
# Release artifact layout. Releases are produced in a later milestone; for tag
# vX.Y.Z the installer expects, under <download_base>/vX.Y.Z/:
#
#   git-remote-sign_X.Y.Z_<os>_<arch>.tar.gz   (contains the git-remote-sign binary)
#   checksums.txt                              (sha256sum format)
#
# where <os> is linux or darwin and <arch> is amd64 or arm64. The artifact is
# verified with `sha256sum -c` before anything is extracted or installed, so an
# unverified download is never executed. Set GIT_REMOTE_SIGNER_BIN to install a
# locally built binary instead.
set -eu

umask 022

BIN_NAME=git-remote-sign

url=${SIGNER_URL:-}
pinned_input=${SIGNER_PUBLIC_KEY:-}
dev_name=${SIGNER_COMMITTER_NAME:-}
dev_email=${SIGNER_COMMITTER_EMAIL:-}

local_bin=${GIT_REMOTE_SIGNER_BIN:-}
release_version=${GIT_REMOTE_SIGNER_RELEASE_VERSION:-v0.1.0}
download_base=${GIT_REMOTE_SIGNER_DOWNLOAD_BASE:-https://github.com/mitchelkuijpers/remote-git-commit-signing/releases/download}
timeout=${SIGNER_TIMEOUT:-}

home=${HOME:-}
if [ -z "$home" ]; then
	echo "install-client: error: HOME is not set; cannot determine where to install." >&2
	exit 1
fi

install_dir=${GIT_REMOTE_SIGNER_INSTALL_DIR:-$home/.local/bin}
config_dir=${GIT_REMOTE_SIGNER_CONFIG_DIR:-${XDG_CONFIG_HOME:-$home/.config}/git-remote-signer}
profile=${GIT_REMOTE_SIGNER_PROFILE:-$home/.profile}
pubkey_path=$config_dir/signing.pub
allowed_signers_path=$config_dir/allowed_signers
env_file=$config_dir/env

skip_selftest=0

usage() {
	cat <<'EOF'
Usage: install-client.sh [--skip-selftest]

Provision this VM for remote Git commit signing. Configuration is taken from
the environment; see the header of this script for the full list.

Options:
  --skip-selftest   configure everything but do not run the signing self-test
  -h, --help        show this help
EOF
}

die() {
	echo "install-client: error: $*" >&2
	exit 1
}

# stage_and_mv SRC DST MODE installs SRC at DST with MODE atomically, staging
# through a temporary file in DST's directory and renaming it into place so a
# partially written file is never observable. It is idempotent: when DST already
# has byte-identical content, it is left untouched.
stage_and_mv() {
	sm_src=$1
	sm_dst=$2
	sm_mode=$3
	sm_tmp=$sm_dst.tmp.$$
	if [ -f "$sm_dst" ] && cmp -s "$sm_src" "$sm_dst"; then
		return 0
	fi
	cp "$sm_src" "$sm_tmp" || die "cannot stage $sm_tmp"
	chmod "$sm_mode" "$sm_tmp" || die "cannot set mode on $sm_tmp"
	mv -f "$sm_tmp" "$sm_dst" || die "cannot install $sm_dst"
}

while [ $# -gt 0 ]; do
	case $1 in
	--skip-selftest)
		skip_selftest=1
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "install-client: error: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

# 1. Required tools and configuration.
need() {
	command -v "$1" >/dev/null 2>&1 ||
		die "required tool not found: $1. Install it first; this script needs no other runtime."
}
need git
need ssh-keygen
need curl
need awk
need sed
need mktemp

[ -n "$url" ] || die "SIGNER_URL is required (the signer base URL, e.g. the exe.dev peer-integration URL)."
case $url in
http://* | https://*) ;;
*) die "SIGNER_URL must start with http:// or https:// (got: $url)." ;;
esac
url=${url%/}

[ -n "$pinned_input" ] || die "SIGNER_PUBLIC_KEY is required (a pinned public key line, or a path to a file containing one)."
[ -n "$dev_name" ] || die "SIGNER_COMMITTER_NAME is required (the developer name for git user.name)."
[ -n "$dev_email" ] || die "SIGNER_COMMITTER_EMAIL is required (the developer email for git user.email)."

WORK=$(mktemp -d "${TMPDIR:-/tmp}/install-client.XXXXXX") ||
	die "cannot create a temporary directory (set TMPDIR to a writable directory)."
trap 'rm -rf "$WORK"' EXIT INT TERM

# 2. Resolve and validate the pinned public key.
if [ -f "$pinned_input" ]; then
	cp "$pinned_input" "$WORK/pinned.raw" ||
		die "cannot read SIGNER_PUBLIC_KEY file: $pinned_input"
else
	printf '%s\n' "$pinned_input" >"$WORK/pinned.raw"
fi
awk 'NF >= 2 { print; exit }' "$WORK/pinned.raw" >"$WORK/pinned.line"
[ -s "$WORK/pinned.line" ] ||
	die "SIGNER_PUBLIC_KEY is neither a readable file nor a public key line: $pinned_input"
if ! ssh-keygen -lf "$WORK/pinned.line" >/dev/null 2>&1; then
	die "SIGNER_PUBLIC_KEY does not contain a valid OpenSSH public key: $pinned_input"
fi
awk 'NF >= 2 { print $1 " " $2; exit }' "$WORK/pinned.line" >"$WORK/pinned.id"

# 3. Reachability, then a hard cross-check of the pinned key.
# Redirects are followed (-L): the exe.dev edge 301s http->https, and without
# it healthz passes on the redirect itself while the key fetching compares
# against the redirect's HTML body. The cross-check below still pins on the
# final key content, so following a hostile redirect can only fail closed.
echo "==> checking signer reachability at $url/healthz" >&2
if ! curl -fsSL --max-time 10 "$url/healthz" >/dev/null 2>&1; then
	die "signer is not reachable at $url (GET /healthz failed). Check SIGNER_URL, that the signer VM is running, and that the exe.dev peer integration is attached to this VM."
fi

echo "==> cross-checking the pinned key against $url/v1/public-key" >&2
if ! curl -fsSL --max-time 10 "$url/v1/public-key" >"$WORK/remote.pub" 2>"$WORK/remote.err"; then
	cat "$WORK/remote.err" >&2 2>/dev/null || true
	die "could not fetch the server public key from $url/v1/public-key; refusing to configure a client whose key cannot be cross-checked."
fi
awk 'NF >= 2 { print $1 " " $2; exit }' "$WORK/remote.pub" >"$WORK/remote.id"
[ -s "$WORK/remote.id" ] || die "the server public key response is not a valid OpenSSH public key."
if ! cmp -s "$WORK/pinned.id" "$WORK/remote.id"; then
	echo "install-client: error: pinned public key does not match the signer's public key" >&2
	echo "  pinned: $(ssh-keygen -lf "$WORK/pinned.line" 2>/dev/null || cat "$WORK/pinned.id")" >&2
	echo "  server: $(ssh-keygen -lf "$WORK/remote.pub" 2>/dev/null || cat "$WORK/remote.id")" >&2
	die "refusing to configure a client with a key the signer does not hold (is a key rotation in progress?)."
fi

# 4. Install the binary: local path first, otherwise a verified download.
install_binary() {
	target=$install_dir/$BIN_NAME
	mkdir -p "$install_dir" || die "cannot create $install_dir"
	if [ -n "$local_bin" ]; then
		[ -f "$local_bin" ] || die "GIT_REMOTE_SIGNER_BIN is not a file: $local_bin"
		[ -x "$local_bin" ] || die "GIT_REMOTE_SIGNER_BIN is not executable: $local_bin"
		if [ -f "$target" ] && cmp -s "$local_bin" "$target"; then
			echo "==> git-remote-sign already installed at $target" >&2
			return 0
		fi
		stage_and_mv "$local_bin" "$target" 0755
		echo "==> installed git-remote-sign from $local_bin" >&2
		return 0
	fi
	download_binary
}

download_binary() {
	need sha256sum
	need tar
	need find
	need sed

	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case $os in
	linux | darwin) ;;
	*) die "unsupported OS: $os (linux and darwin release artifacts are published)." ;;
	esac
	machine=$(uname -m)
	case $machine in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $machine (amd64 and arm64 release artifacts are published)." ;;
	esac

	stem=git-remote-sign_${release_version#v}_${os}_${arch}
	artifact=$stem.tar.gz
	base=$download_base/$release_version
	dl=$WORK/download
	mkdir -p "$dl"

	echo "==> downloading $artifact" >&2
	curl -fsSL --max-time 300 -o "$dl/$artifact" "$base/$artifact" ||
		die "download failed: $base/$artifact. No release artifacts exist until the release is published; set GIT_REMOTE_SIGNER_BIN to install a local binary."
	curl -fsSL --max-time 60 -o "$dl/checksums.txt" "$base/checksums.txt" ||
		die "download failed: $base/checksums.txt"

	awk -v a="$artifact" '$2 == a' "$dl/checksums.txt" >"$dl/$artifact.sha256"
	[ -s "$dl/$artifact.sha256" ] ||
		die "checksums.txt has no entry for $artifact; refusing to install an unverified download."
	if ! (cd "$dl" && sha256sum -c "$dl/$artifact.sha256" >"$dl/verify.out" 2>&1); then
		cat "$dl/verify.out" >&2
		die "checksum verification failed for $artifact; refusing to install."
	fi
	echo "==> checksum verified ($(awk '{ print $1 }' "$dl/$artifact.sha256"))" >&2

	extract=$dl/extract
	mkdir -p "$extract"
	tar -xzf "$dl/$artifact" -C "$extract" || die "cannot extract $artifact"
	src=$(find "$extract" -type f -name "$BIN_NAME" | sed -n '1p')
	[ -n "$src" ] || die "$artifact does not contain a $BIN_NAME binary."
	stage_and_mv "$src" "$install_dir/$BIN_NAME" 0755
	echo "==> installed git-remote-sign from verified release $release_version" >&2
}

install_binary

# 5. Install the pinned public key and the local allowed-signers file (public
# material only). The allowed-signers file maps the pinned committer email to
# the pinned key so that local `git verify-commit` trusts signatures without any
# manual configuration.
if [ ! -d "$config_dir" ]; then
	mkdir -p "$config_dir" || die "cannot create $config_dir"
	chmod 0700 "$config_dir"
fi
if [ -f "$pubkey_path" ] && cmp -s "$WORK/pinned.line" "$pubkey_path"; then
	echo "==> pinned public key already installed at $pubkey_path" >&2
else
	stage_and_mv "$WORK/pinned.line" "$pubkey_path" 0644
	echo "==> installed pinned public key at $pubkey_path" >&2
fi

# The principal is the committer email Git verifies against; the key is the
# pinned key, reduced to "type blob" (comments are ignored).
printf '%s %s\n' "$dev_email" "$(cat "$WORK/pinned.id")" >"$WORK/allowed.line"
if [ -f "$allowed_signers_path" ] && cmp -s "$WORK/allowed.line" "$allowed_signers_path"; then
	echo "==> allowed-signers file already installed at $allowed_signers_path" >&2
else
	stage_and_mv "$WORK/allowed.line" "$allowed_signers_path" 0644
	echo "==> installed allowed-signers file at $allowed_signers_path" >&2
fi

# 6. User-level git configuration. Only these keys are set; everything else in
# the user's git config is left untouched.
set_git_config() {
	git config --global "$1" "$2" || die "git config --global $1 failed"
}
echo "==> configuring user-level git signing" >&2
set_git_config gpg.format ssh
set_git_config gpg.ssh.program "$install_dir/$BIN_NAME"
set_git_config commit.gpgsign true
set_git_config user.signingkey "$pubkey_path"
set_git_config gpg.ssh.allowedSignersFile "$allowed_signers_path"
set_git_config user.name "$dev_name"
set_git_config user.email "$dev_email"

# 7. Persist the two client environment variables. The client reads them from
# the environment, so they are written to a file that the login profile sources.
quote() {
	printf "'"
	printf '%s' "$1" | sed "s/'/'\\\\''/g"
	printf "'"
}
env_src=$WORK/env.content
{
	printf '%s\n' "# Managed by deploy/install-client.sh; re-running the installer"
	printf '%s\n' "# rewrites this file. It configures git-remote-sign (no key material)."
	printf 'export SIGNER_URL=%s\n' "$(quote "$url")"
	printf 'export SIGNER_PUBLIC_KEY=%s\n' "$(quote "$pubkey_path")"
	if [ -n "$timeout" ]; then
		printf 'export SIGNER_TIMEOUT=%s\n' "$(quote "$timeout")"
	fi
} >"$env_src" || die "cannot write $env_src"
stage_and_mv "$env_src" "$env_file" 0644

begin_marker="# >>> git-remote-signer >>>"
end_marker="# <<< git-remote-signer <<<"
block=$WORK/profile.block
{
	printf '%s\n' "$begin_marker"
	printf 'if [ -f %s ]; then\n' "$(quote "$env_file")"
	printf '\t. %s\n' "$(quote "$env_file")"
	printf '%s\n' "fi"
	printf '%s\n' "$end_marker"
} >"$block"
profile_new=$WORK/profile.new
if [ -f "$profile" ]; then
	awk -v b="$begin_marker" -v e="$end_marker" '
		$0 == b { skip = 1; next }
		$0 == e { skip = 0; next }
		skip { next }
		{ print }
	' "$profile" >"$profile_new" || die "cannot read $profile"
else
	: >"$profile_new"
fi
cat "$block" >>"$profile_new"
if [ -f "$profile" ] && cmp -s "$profile_new" "$profile"; then
	:
else
	prof_dir=$(dirname -- "$profile")
	[ -d "$prof_dir" ] || mkdir -p "$prof_dir" || die "cannot create $prof_dir"
	stage_and_mv "$profile_new" "$profile" 0644
	echo "==> client environment persisted in $env_file (sourced from $profile)" >&2
fi

# 8. Harmless signing self-test: a throwaway repository, no remote, removed
# afterwards. This exercises the real installed binary and pinned key against
# the real signer, exactly as an ordinary `git commit` would.
selftest() {
	echo "==> running signing self-test in a temporary repository" >&2
	st=$WORK/selftest
	repo=$st/repo
	mkdir -p "$st"
	git init -q -b main "$repo" || die "self-test: git init failed"
	git -C "$repo" config user.name "$dev_name"
	git -C "$repo" config user.email "$dev_email"
	git -C "$repo" config gpg.format ssh
	git -C "$repo" config commit.gpgsign true
	git -C "$repo" config gpg.ssh.program "$install_dir/$BIN_NAME"
	git -C "$repo" config user.signingkey "$pubkey_path"
	# Verification uses the allowed-signers file the installer just wrote to the
	# global config, exercising the real provisioning end to end.
	printf 'self-test\n' >"$repo/README.md"
	git -C "$repo" add README.md
	if ! SIGNER_URL="$url" SIGNER_PUBLIC_KEY="$pubkey_path" \
		git -C "$repo" commit -q -m "selftest: remote signing round-trip" >"$st/commit.out" 2>&1; then
		cat "$st/commit.out" >&2
		die "self-test commit failed. The signer is reachable, so check that SIGNER_URL is the platform URL that injects the verified VM identity for POST /v1/sign, that this VM is in SIGNER_ALLOWLIST, and that the committer identity matches SIGNER_COMMITTER_NAME/EMAIL."
	fi
	if ! SIGNER_URL="$url" SIGNER_PUBLIC_KEY="$pubkey_path" \
		git -C "$repo" verify-commit HEAD >"$st/verify.out" 2>&1; then
		cat "$st/verify.out" >&2
		die "self-test signature did not verify against the pinned key."
	fi
	cat "$st/verify.out" >&2
	echo "==> self-test passed: commit signed through the signer and verified locally" >&2
}

if [ "$skip_selftest" -eq 0 ]; then
	selftest
else
	echo "==> skipping the signing self-test (--skip-selftest)" >&2
fi

echo "" >&2
echo "client installed:" >&2
echo "  binary:     $install_dir/$BIN_NAME" >&2
echo "  pinned key: $pubkey_path" >&2
echo "  allowed:    $allowed_signers_path" >&2
echo "  git config: gpg.format=ssh gpg.ssh.program=$install_dir/$BIN_NAME commit.gpgsign=true user.signingkey=$pubkey_path gpg.ssh.allowedSignersFile=$allowed_signers_path" >&2
echo "  env:        $env_file (sourced from $profile)" >&2
if [ "$skip_selftest" -eq 0 ]; then
	echo "  self-test:  passed" >&2
else
	echo "  self-test:  skipped" >&2
fi
