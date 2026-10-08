#!/usr/bin/env bash
#
# Milestone 1 local demo (one command).
#
# Proves the whole loop on one machine: generate a throwaway signing key, start
# git-signer-server, point an ordinary Git repository at git-remote-sign, make a
# normal `git commit`, and verify its embedded SSHSIG with stock Git.
#
# Everything happens inside a temporary directory; nothing outside it is
# modified and no key material is written to the repository.
#
# Production note: git-signer-server requires the platform-verified source-VM
# identity on POST /v1/sign. exe.dev's authenticated peer proxy supplies it in
# production; this demo starts scripts/devproxy to stand in for that proxy and
# stamp the header locally.
#
# Usage: scripts/demo-local.sh
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
server_pid=""
proxy_pid=""

cleanup() {
	[ -n "$proxy_pid" ] && kill "$proxy_pid" 2>/dev/null || true
	[ -n "$server_pid" ] && kill "$server_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT

need() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "demo: required tool not found: $1" >&2
		exit 1
	}
}
need go
need git
need ssh-keygen
need curl

# free_port prints a currently-unused TCP port on loopback.
free_port() {
	local p
	for _ in $(seq 1 100); do
		p=$((20000 + RANDOM % 30000))
		if ! (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
			echo "$p"
			return 0
		fi
	done
	echo 8000
}

name="Demo Agent"
email="demo@example.com"
vm="local-demo-vm"
server_port="${SIGNER_PORT:-$(free_port)}"

echo "==> building binaries"
bin="$work/bin"
mkdir -p "$bin"
(cd "$root" && go build -o "$bin" \
	./cmd/git-signer-server ./cmd/git-remote-sign ./scripts/devproxy)

echo "==> generating throwaway ED25519 signing key"
ssh-keygen -q -t ed25519 -N '' -C git-signer-demo -f "$work/signing_key"
pub="$work/signing_key.pub"
ssh-keygen -y -f "$work/signing_key" >"$pub"

echo "==> starting signer server on 127.0.0.1:$server_port"
SIGNER_KEY_PATH="$work/signing_key" \
	SIGNER_PORT="$server_port" \
	SIGNER_COMMITTER_NAME="$name" \
	SIGNER_COMMITTER_EMAIL="$email" \
	SIGNER_ALLOWLIST="$vm" \
	"$bin/git-signer-server" >"$work/server.log" 2>&1 &
server_pid=$!

url="http://127.0.0.1:$server_port"
ready=""
for _ in $(seq 1 100); do
	if curl -fsS "$url/readyz" >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 0.1
done
if [ -z "$ready" ]; then
	echo "demo: signer server did not become ready; log:" >&2
	cat "$work/server.log" >&2
	exit 1
fi

echo "==> starting platform shim (stamps X-Exedev-Source-Vm) in front of the signer"
"$bin/devproxy" -listen 127.0.0.1:0 -upstream "$url" -vm "$vm" \
	>"$work/proxy.out" 2>"$work/proxy.log" &
proxy_pid=$!

proxy_addr=""
for _ in $(seq 1 100); do
	if grep -q '^listening ' "$work/proxy.out" 2>/dev/null; then
		proxy_addr="$(awk '/^listening /{print $2; exit}' "$work/proxy.out")"
		break
	fi
	sleep 0.1
done
if [ -z "$proxy_addr" ]; then
	echo "demo: platform shim did not start; log:" >&2
	cat "$work/proxy.log" >&2
	exit 1
fi
proxy_url="http://$proxy_addr"

echo "==> configuring a temporary Git repository"
repo="$work/repo"
git init -q -b main "$repo"
# Keep the demo hermetic: no user or system Git configuration leaks in.
export HOME="$work"
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_REMOTE_SIGNER_URL="$proxy_url"
export GIT_REMOTE_SIGNER_PUBLIC_KEY="$pub"
git -C "$repo" config user.name "$name"
git -C "$repo" config user.email "$email"
git -C "$repo" config gpg.format ssh
git -C "$repo" config commit.gpgsign true
git -C "$repo" config gpg.ssh.program "$bin/git-remote-sign"
git -C "$repo" config user.signingkey "$pub"

# Git needs an allowed-signers file to trust the signature at verify time.
# Principal = the committer email; key = the public key just generated.
printf '%s %s %s\n' "$email" $(awk '{print $1, $2}' "$pub") >"$work/allowed_signers"
git -C "$repo" config gpg.ssh.allowedSignersFile "$work/allowed_signers"

echo "==> committing (Git signs through git-remote-sign -> signer server)"
printf 'hello from the milestone 1 demo\n' >"$repo/README.md"
git -C "$repo" add README.md
git -C "$repo" commit -q -m "demo: signed commit"

echo "==> verifying the commit signature with stock Git"
git -C "$repo" verify-commit HEAD
echo
echo "demo: OK - commit signed through the remote signer and verified locally"
