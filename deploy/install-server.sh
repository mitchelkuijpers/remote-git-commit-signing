#!/bin/sh
#
# install-server.sh -- install git-signer-server as a hardened systemd service
# on a dedicated unprivileged account.
#
# It creates the service user and the key directory with restrictive
# permissions, installs the binary and the unit file, generates a signing key
# if none exists yet (delegating to generate-key.sh), and enables + starts the
# service. Re-running is safe: an existing key is never touched.
#
# --no-start and DESTDIR are intentional: they install the same files
# idempotently without creating users, chowning, or starting the service, so
# non-root/CI staging and image builds can exercise the installer.
#
# Usage: sudo deploy/install-server.sh [--no-start] [--skip-key]
#
# Environment:
#   SIGNER_USER            service account      (default: git-signer)
#   SIGNER_GROUP           service group        (default: same as user)
#   SIGNER_KEY_DIR         key directory        (default: /var/lib/git-signer)
#   SIGNER_KEY_NAME        key file name        (default: signing_key)
#   SIGNER_CONF_DIR        env-file directory   (default: /etc/git-signer)
#   SIGNER_COMMITTER_NAME  committer name written to the env file (required)
#   SIGNER_COMMITTER_EMAIL committer email written to the env file (required)
#   SIGNER_ALLOWLIST       VM identities allowed to sign, comma-separated
#                              exact names or globs (required; empty denies all)
#   SIGNER_PORT            listen port written to the env file (default 8000)
#   SIGNER_RATE_PER_MIN    sustained per-VM signing rate (default: server's)
#   SIGNER_RATE_BURST      per-VM burst capacity (default: server's)
#   SIGNER_DIST_DIR        directory the server serves client downloads
#                              from (default: /usr/local/lib/git-signer/dist);
#                              client binaries are built from SIGNER_REPO_DIR
#   SIGNER_SKIP_DIST       set to 1 to skip building/installing the client
#                              dist (disables /install.sh and /v1/client/...)
#   SIGNER_SERVER_BIN      prebuilt server binary to install
#   SIGNER_REPO_DIR        source tree used to build the binary
#   DESTDIR                    staging root; staged installs skip user
#                              creation, chown, and systemctl so they can run
#                              without root (for packaging/image builds)
#
# If SIGNER_CONF_DIR is changed, update EnvironmentFile= in the unit too.
#
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=${SIGNER_REPO_DIR:-$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)}

SIGNER_USER=${SIGNER_USER:-git-signer}
SIGNER_GROUP=${SIGNER_GROUP:-$SIGNER_USER}
KEY_DIR=${SIGNER_KEY_DIR:-/var/lib/git-signer}
KEY_NAME=${SIGNER_KEY_NAME:-signing_key}
CONF_DIR=${SIGNER_CONF_DIR:-/etc/git-signer}
DESTDIR=${DESTDIR:-}

BIN_NAME=git-signer-server
UNIT_NAME=git-signer.service
CLIENT_BIN_NAME=git-remote-sign
DIST_DIR=${SIGNER_DIST_DIR:-/usr/local/lib/git-signer/dist}
skip_dist=${SIGNER_SKIP_DIST:-0}

cfg_key_path=$KEY_DIR/$KEY_NAME
cfg_dist_dir=$DIST_DIR

# Shared build scratch for everything this run compiles; removed on exit.
work_dir=$(mktemp -d)
# shellcheck disable=SC2064 # expand work_dir when the trap runs
trap 'rm -rf "$work_dir"' EXIT INT TERM

staging=0
if [ -n "$DESTDIR" ]; then
	staging=1
fi

no_start=0
skip_key=0

usage() {
	cat <<'EOF'
Usage: install-server.sh [--no-start] [--skip-key]

Install git-signer-server as a systemd service (must run as root).

Options:
  --no-start   install files and reload systemd, but do not enable/start
  --skip-key   do not generate a signing key if none exists
  -h, --help   show this help

Set DESTDIR to stage the install into a temporary root (no root required).
EOF
}

while [ $# -gt 0 ]; do
	case $1 in
	--no-start)
		no_start=1
		;;
	--skip-key)
		skip_key=1
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

# Filesystem view: in staging mode everything lands under DESTDIR.
fs_key_dir=$KEY_DIR
fs_conf_dir=$CONF_DIR
fs_dist_dir=$DIST_DIR
bin_dir=/usr/local/bin
unit_dir=/etc/systemd/system
if [ "$staging" -eq 1 ]; then
	fs_key_dir=$DESTDIR$KEY_DIR
	fs_conf_dir=$DESTDIR$CONF_DIR
	fs_dist_dir=$DESTDIR$DIST_DIR
	bin_dir=$DESTDIR/usr/local/bin
	unit_dir=$DESTDIR/etc/systemd/system
fi
fs_key_path=$fs_key_dir/$KEY_NAME
fs_env_file=$fs_conf_dir/git-signer.env

if [ "$staging" -eq 0 ]; then
	if [ "$(id -u)" -ne 0 ]; then
		echo "error: install-server.sh must run as root (creating users, /usr/local/bin, and systemd units)." >&2
		echo "       Re-run with sudo, or set DESTDIR to stage the install without root." >&2
		exit 1
	fi
	if ! command -v systemctl >/dev/null 2>&1; then
		echo "error: systemctl not found; this installer targets systemd hosts." >&2
		exit 1
	fi
fi

# A real deployment needs the full configuration before the service can be
# useful: without a committer identity commits cannot be attributed, and
# without an allowlist the server (correctly) denies every VM. Staged installs
# skip this so images can be built before the values are known.
if [ "$staging" -eq 0 ]; then
	missing=
	if [ -z "${SIGNER_COMMITTER_NAME:-}" ]; then missing="$missing SIGNER_COMMITTER_NAME"; fi
	if [ -z "${SIGNER_COMMITTER_EMAIL:-}" ]; then missing="$missing SIGNER_COMMITTER_EMAIL"; fi
	if [ -z "${SIGNER_ALLOWLIST:-}" ]; then missing="$missing SIGNER_ALLOWLIST"; fi
	if [ -n "$missing" ]; then
		echo "error: missing required configuration:$missing" >&2
		echo "       Re-run, for example:" >&2
		echo "         sudo env SIGNER_COMMITTER_NAME='Your Name' \\" >&2
		echo "           SIGNER_COMMITTER_EMAIL='you@example.com' \\" >&2
		echo "           SIGNER_ALLOWLIST='agent-*' deploy/install-server.sh" >&2
		echo "       (SIGNER_ALLOWLIST is fail-closed: an empty value admits nobody.)" >&2
		exit 1
	fi
fi

unit_src=${SIGNER_UNIT_FILE:-$SCRIPT_DIR/$UNIT_NAME}
if [ ! -f "$unit_src" ]; then
	echo "error: systemd unit not found: $unit_src" >&2
	exit 1
fi

# resolve_binary sets bin_src to an installable server binary. It sets a global
# (rather than printing) so that a binary built into a temporary directory is
# not removed by the subshell that command substitution would create.
bin_src=
resolve_binary() {
	if [ -n "${SIGNER_SERVER_BIN:-}" ]; then
		if [ -x "$SIGNER_SERVER_BIN" ]; then
			bin_src=$SIGNER_SERVER_BIN
			return 0
		fi
		echo "error: SIGNER_SERVER_BIN is not an executable file: $SIGNER_SERVER_BIN" >&2
		return 1
	fi
	for candidate in "$SCRIPT_DIR/$BIN_NAME" "$REPO_DIR/$BIN_NAME"; do
		if [ -x "$candidate" ]; then
			bin_src=$candidate
			return 0
		fi
	done
	if command -v go >/dev/null 2>&1; then
		echo "building $BIN_NAME from $REPO_DIR" >&2
		if (cd "$REPO_DIR" && go build -o "$work_dir/$BIN_NAME" "./cmd/$BIN_NAME"); then
			bin_src=$work_dir/$BIN_NAME
			return 0
		fi
		echo "error: go build failed" >&2
		return 1
	fi
	echo "error: cannot find the $BIN_NAME binary." >&2
	echo "       Build it with 'go build -o $BIN_NAME ./cmd/$BIN_NAME' or set SIGNER_SERVER_BIN." >&2
	return 1
}

resolve_binary

# build_client_dist cross-compiles the client for both Linux architectures
# into work_dir/dist and adds install-client.sh, so the server can serve the
# whole client bootstrap itself. Skew between the installed server and the
# served client is practically impossible because both come from the same
# source tree in the same installer run.
build_client_dist() {
	if [ "$skip_dist" = "1" ]; then
		echo "note: SIGNER_SKIP_DIST=1; client dist not installed" >&2
		echo "      (/install.sh and /v1/client/... will not work)" >&2
		return 0
	fi
	if ! command -v go >/dev/null 2>&1; then
		echo "error: go not found; cannot build the client dist." >&2
		echo "       Install Go or set SIGNER_SKIP_DIST=1 to skip." >&2
		return 1
	fi
	mkdir -p "$work_dir/dist"
	for arch in amd64 arm64; do
		echo "building $CLIENT_BIN_NAME-linux-$arch from $REPO_DIR" >&2
		if ! (cd "$REPO_DIR" && GOOS=linux GOARCH=$arch go build -o "$work_dir/dist/$CLIENT_BIN_NAME-linux-$arch" "./cmd/$CLIENT_BIN_NAME"); then
			echo "error: go build failed (linux/$arch client)" >&2
			return 1
		fi
	done
	cp "$SCRIPT_DIR/install-client.sh" "$work_dir/dist/install-client.sh"
}

build_client_dist

# 1. Dedicated unprivileged account.
if [ "$staging" -eq 0 ]; then
	if ! id -u "$SIGNER_USER" >/dev/null 2>&1; then
		echo "creating system user $SIGNER_USER" >&2
		if ! command -v useradd >/dev/null 2>&1; then
			echo "error: useradd not found; create the user $SIGNER_USER manually first" >&2
			exit 1
		fi
		useradd --system --no-create-home --home-dir "$KEY_DIR" --shell /usr/sbin/nologin "$SIGNER_USER"
	fi
	if ! getent group "$SIGNER_GROUP" >/dev/null 2>&1; then
		if ! command -v groupadd >/dev/null 2>&1; then
			echo "error: groupadd not found; create the group $SIGNER_GROUP manually first" >&2
			exit 1
		fi
		groupadd --system "$SIGNER_GROUP"
	fi
fi

# 2. Directory layout with restrictive permissions.
if [ "$staging" -eq 0 ]; then
	install -d -m 0700 -o "$SIGNER_USER" -g "$SIGNER_GROUP" "$fs_key_dir"
else
	mkdir -p "$fs_key_dir"
	chmod 0700 "$fs_key_dir"
fi
install -d -m 0755 "$fs_conf_dir"

# 3. Env file (contains configuration only -- never key material).
#
# emit writes NAME=value, or a commented REQUIRED placeholder when the value is
# not supplied (staged installs only; real installs validated it above).
emit() {
	if [ -n "$2" ]; then
		echo "$1=$2"
	else
		echo "# $1=   # REQUIRED: set before starting the service"
	fi
}

umask 027
tmp_env=$fs_env_file.tmp
{
	echo "# Managed by deploy/install-server.sh; re-running the installer"
	echo "# overwrites this file. The private signing key is read from disk by"
	echo "# path -- never put key material in this file or in the environment."
	echo "SIGNER_KEY_PATH=$cfg_key_path"
	emit SIGNER_COMMITTER_NAME "${SIGNER_COMMITTER_NAME:-}"
	emit SIGNER_COMMITTER_EMAIL "${SIGNER_COMMITTER_EMAIL:-}"
	emit SIGNER_ALLOWLIST "${SIGNER_ALLOWLIST:-}"
	if [ "$skip_dist" = "1" ]; then
		echo "#SIGNER_DIST_DIR=   # unset: client dist serving disabled"
	else
		echo "SIGNER_DIST_DIR=$cfg_dist_dir"
	fi
	echo "# Optional; uncomment to override the server defaults."
	if [ -n "${SIGNER_URL:-}" ]; then
		echo "SIGNER_URL=$SIGNER_URL"
	else
		echo "#SIGNER_URL=https://git-signer.int.exe.xyz"
	fi
	if [ -n "${SIGNER_PORT:-}" ]; then
		echo "SIGNER_PORT=$SIGNER_PORT"
	else
		echo "#SIGNER_PORT=8000"
	fi
	if [ -n "${SIGNER_RATE_PER_MIN:-}" ]; then
		echo "SIGNER_RATE_PER_MIN=$SIGNER_RATE_PER_MIN"
	else
		echo "#SIGNER_RATE_PER_MIN=60"
	fi
	if [ -n "${SIGNER_RATE_BURST:-}" ]; then
		echo "SIGNER_RATE_BURST=$SIGNER_RATE_BURST"
	else
		echo "#SIGNER_RATE_BURST=10"
	fi
} >"$tmp_env"
chmod 0640 "$tmp_env"
if [ "$staging" -eq 0 ]; then
	chown root:"$SIGNER_GROUP" "$tmp_env"
fi
mv -f "$tmp_env" "$fs_env_file"
if [ "$staging" -eq 1 ]; then
	if [ -z "${SIGNER_COMMITTER_NAME:-}" ] || [ -z "${SIGNER_COMMITTER_EMAIL:-}" ] || [ -z "${SIGNER_ALLOWLIST:-}" ]; then
		echo "note: staged install with placeholder configuration; fill in the" >&2
		echo "      REQUIRED entries in $fs_env_file before starting the service." >&2
	fi
fi

# 4. Binary and unit.
install -d -m 0755 "$bin_dir" "$unit_dir"
install -m 0755 "$bin_src" "$bin_dir/$BIN_NAME"
install -m 0644 "$unit_src" "$unit_dir/$UNIT_NAME"

# 4b. Client dist served by the server (world-readable; no key material — the
# client binaries and the installer script only).
if [ "$skip_dist" != "1" ]; then
	install -d -m 0755 "$fs_dist_dir"
	install -m 0755 "$work_dir/dist/$CLIENT_BIN_NAME-linux-amd64" "$work_dir/dist/$CLIENT_BIN_NAME-linux-arm64" "$work_dir/dist/install-client.sh" "$fs_dist_dir/"
fi

# 5. Signing key (only if absent).
if [ "$skip_key" -eq 1 ]; then
	echo "note: --skip-key set; not generating a signing key" >&2
elif [ ! -f "$fs_key_path" ]; then
	echo "no signing key at $fs_key_path; generating one" >&2
	skip_chown=0
	if [ "$staging" -eq 1 ]; then
		skip_chown=1
	fi
	SIGNER_KEY_DIR=$fs_key_dir \
		SIGNER_KEY_NAME=$KEY_NAME \
		SIGNER_USER=$SIGNER_USER \
		SIGNER_GROUP=$SIGNER_GROUP \
		SIGNER_SKIP_CHOWN=$skip_chown \
		"$SCRIPT_DIR/generate-key.sh" >/dev/null
fi

# 6. systemd.
if [ "$staging" -eq 1 ]; then
	echo "staged install: skipping systemctl (start with systemctl enable --now $UNIT_NAME)" >&2
else
	systemctl daemon-reload
	if [ "$no_start" -eq 1 ]; then
		echo "installed; start with: systemctl enable --now $UNIT_NAME" >&2
	else
		systemctl enable "$UNIT_NAME" >/dev/null 2>&1 ||
			echo "warning: could not enable $UNIT_NAME" >&2
		systemctl restart "$UNIT_NAME"
		sleep 1
		if ! systemctl is-active --quiet "$UNIT_NAME"; then
			echo "error: $UNIT_NAME is not active; recent journal:" >&2
			journalctl -u "$UNIT_NAME" -n 20 --no-pager >&2 || true
			exit 1
		fi
		echo "started $UNIT_NAME" >&2
	fi
fi

# 7. Summary.
echo "" >&2
echo "public signing key (register with GitLab as a Signing key only):" >&2
cat "$fs_key_path.pub"
echo "" >&2
if [ "$skip_dist" != "1" ]; then
	echo "client bootstrap (on any attached agent VM):" >&2
	echo "  curl -fsSL ${SIGNER_URL:-https://git-signer.int.exe.xyz}/install.sh | sh" >&2
	echo "" >&2
fi
echo "logs:      journalctl -u $UNIT_NAME -f" >&2
echo "lifecycle: docs/key-lifecycle.md (backup, rotation, recovery)" >&2
