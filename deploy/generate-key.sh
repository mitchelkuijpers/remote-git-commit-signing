#!/bin/sh
#
# generate-key.sh -- create the ED25519 signing key used by git-signer-server.
#
# The key is written into the root-only key directory with mode 0600 and owned
# by the unprivileged service account. It is generated *without* a passphrase
# so the server can sign unattended; confidentiality relies on file permissions
# and an offline backup (see docs/key-lifecycle.md). A passphrase would not add
# security here: the server has no terminal to unlock the key.
#
# The script never prints private key material: only the public key line goes
# to stdout, everything else goes to stderr.
#
# Usage: sudo deploy/generate-key.sh [--force]
#
set -eu

KEY_DIR=${SIGNER_KEY_DIR:-/var/lib/git-signer}
KEY_NAME=${SIGNER_KEY_NAME:-signing_key}
KEY_USER=${SIGNER_USER:-git-signer}
KEY_GROUP=${SIGNER_GROUP:-$KEY_USER}
KEY_COMMENT=${SIGNER_KEY_COMMENT:-git-signer remote signing key}

KEY_PATH=$KEY_DIR/$KEY_NAME
PUB_PATH=$KEY_PATH.pub

force=0
chown_owner=1
# SIGNER_SKIP_CHOWN=1 is for local/testing use only: it generates a key
# owned by the invoking user instead of the service account.
if [ "${SIGNER_SKIP_CHOWN:-0}" = 1 ]; then
	chown_owner=0
fi

usage() {
	cat <<'EOF'
Usage: generate-key.sh [--force]

Generate the ED25519 signing key for git-signer-server.

Options:
  -f, --force   rotate: move an existing key aside to <name>.old first
  -h, --help    show this help

Environment:
  SIGNER_KEY_DIR     key directory (default: /var/lib/git-signer)
  SIGNER_KEY_NAME    key file name (default: signing_key)
  SIGNER_USER        owning user (default: git-signer)
  SIGNER_GROUP       owning group (default: same as user)
  SIGNER_KEY_COMMENT comment stored in the public key
  SIGNER_SKIP_CHOWN  if 1, do not change ownership (local testing only)
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	-f | --force)
		force=1
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "error: unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
	shift
done

if [ "$(id -u)" -ne 0 ] && [ "$chown_owner" -eq 1 ]; then
	echo "error: generate-key.sh must run as root: it creates $KEY_DIR and sets ownership to $KEY_USER." >&2
	echo "       Re-run with sudo, or set SIGNER_SKIP_CHOWN=1 for a local, unowned key." >&2
	exit 1
fi

if ! command -v ssh-keygen >/dev/null 2>&1; then
	echo "error: ssh-keygen not found in PATH" >&2
	exit 1
fi

if [ "$chown_owner" -eq 1 ]; then
	if ! id -u "$KEY_USER" >/dev/null 2>&1; then
		echo "error: user $KEY_USER does not exist; run deploy/install-server.sh first" >&2
		exit 1
	fi
	if ! getent group "$KEY_GROUP" >/dev/null 2>&1; then
		echo "error: group $KEY_GROUP does not exist; run deploy/install-server.sh first" >&2
		exit 1
	fi
fi

if [ -e "$KEY_PATH" ]; then
	if [ "$force" -ne 1 ]; then
		echo "error: $KEY_PATH already exists; refusing to overwrite it." >&2
		echo "       Use --force to rotate (the old key is kept as $KEY_PATH.old)." >&2
		exit 1
	fi
	echo "rotating: keeping the previous key as $KEY_PATH.old" >&2
	mv -f "$KEY_PATH" "$KEY_PATH.old"
	if [ -e "$PUB_PATH" ]; then
		mv -f "$PUB_PATH" "$PUB_PATH.old"
	fi
fi

mkdir -p "$KEY_DIR"
chmod 0700 "$KEY_DIR"
if [ "$chown_owner" -eq 1 ]; then
	chown "$KEY_USER:$KEY_GROUP" "$KEY_DIR"
fi

# 077 keeps ssh-keygen from creating anything group/world accessible.
umask 077
if ! gen_out=$(ssh-keygen -q -t ed25519 -N '' -C "$KEY_COMMENT" -f "$KEY_PATH" 2>&1); then
	echo "error: ssh-keygen failed to generate the key: $gen_out" >&2
	exit 1
fi

chmod 0600 "$KEY_PATH"
chmod 0644 "$PUB_PATH"
if [ "$chown_owner" -eq 1 ]; then
	chown "$KEY_USER:$KEY_GROUP" "$KEY_PATH" "$PUB_PATH"
fi

# Verify the private key is usable and derives exactly the public key we ship.
if ! derived_line=$(ssh-keygen -y -f "$KEY_PATH" 2>/dev/null); then
	echo "error: generated key could not be read back by ssh-keygen" >&2
	exit 1
fi
derived_blob=$(printf '%s\n' "$derived_line" | awk '{ print $2 }')
pub_blob=$(awk '{ print $2 }' <"$PUB_PATH")
if [ -z "$derived_blob" ] || [ "$derived_blob" != "$pub_blob" ]; then
	echo "error: public key does not match the private key" >&2
	exit 1
fi

echo "generated $KEY_PATH (mode 0600) and $PUB_PATH" >&2
echo "register the public key with GitLab as a Signing key only; see docs/key-lifecycle.md" >&2
if [ "$force" -eq 1 ]; then
	echo "rotation: restart the service so it loads the new key (it caches the public key at startup):" >&2
	echo "  sudo systemctl restart git-signer.service" >&2
fi

# Print only the public key.
cat "$PUB_PATH"
