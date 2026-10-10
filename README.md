# remote-git-commit-signing

Remote Git commit signing for [exe.dev](https://exe.dev) agents.

A lightweight, centralized SSH commit-signing service that lets dozens of short-lived
agent VMs produce Git commits signed with a single signing key — **without ever putting
the private key on an agent VM**.

## Why

Ephemeral agent VMs are created and destroyed constantly. Distributing a GitLab signing
key to each one creates a key-management problem. This project keeps the private key on
one persistent signer VM and exposes a small, authenticated signing API to agents.

- **One** SSH signing key, registered once with GitLab.
- **Zero** private signing keys on agent VMs.
- **Zero** manually managed credentials between agents and the signer.
- Signing happens automatically with an ordinary `git commit`.
- Minimal per-VM configuration, and nothing to clean up when a VM is destroyed.

## How it works

Two small Go programs:

| Binary | Runs on | Role |
|---|---|---|
| `git-signer-server` | Persistent signer VM | Holds the private key, validates requests, returns SSHSIG signatures |
| `git-remote-sign` | Ephemeral agent VMs | Implements Git's `gpg.ssh.program` interface and calls the signer |

```text
 ┌────────────────────────── agent VM (ephemeral) ────────────────────────────┐
 │  git commit                                                                │
 │     │  gpg.ssh.program = git-remote-sign                                   │
 │     ▼                                                                      │
 │  git-remote-sign ──POST /v1/sign (raw commit payload)──┐                   │
 │     │  ◀──────────── raw SSHSIG ───────────────────────┘                   │
 │     ├─ verify the SSHSIG locally against the pinned public key             │
 │     └─ write <buffer>.sig, exit 0     (any failure → exit non-zero)        │
 └────────────────────────────────────────────────────────────────────────────┘
                                   │
                     exe.dev authenticated peer proxy
                     sets platform-verified X-Exedev-Source-Vm
                     (and strips any client-supplied value)
                                   │
 ┌────────────────────────── signer VM (persistent) ──────────────────────────┐
 │  git-signer-server :8000                                                   │
 │    identity → allowlist → rate limit → commit validation                   │
 │    → ssh-keygen -Y sign (ED25519 private key, never leaves this VM)        │
 │    → raw SSHSIG + one slog JSON audit line (vm, payload SHA-256, status)   │
 └────────────────────────────────────────────────────────────────────────────┘
```

Agent VMs reach the signer through exe.dev's authenticated VM-to-VM peer integration,
which provides a platform-verified caller identity. The private key never leaves the
signer, and there is no unauthenticated fallback anywhere in the path.

## Documentation

- [Architecture](docs/architecture.md) — components, trust boundaries, sign/verify request
  flows, key locations, and the full configuration surface (every env var, both binaries).
- [exe.dev setup](docs/exe-dev-setup.md) — the peer integration (`add http-proxy --peer
  --attach tag:...`) and the per-VM provisioning story.
- [GitLab setup](docs/gitlab-setup.md) — registering the public key as **Signing-only**,
  the verified-email requirement, and the Verified-badge checklist.
- [Security](docs/security.md) — threat model, trust boundaries, the accepted residual
  risk, and what the pinned client key protects against.
- [Troubleshooting](docs/troubleshooting.md) — failure classes with symptoms and fixes,
  verify exit-code semantics, systemd startup failures.
- [Acceptance checklist (manual gate)](docs/acceptance-checklist.md) — the real-GitLab
  end-to-end checklist that must pass before production use.
- [Signing key lifecycle](docs/key-lifecycle.md) — server deployment, key generation,
  GitLab registration, backup, rotation, and recovery.
- [Implementation plan](docs/implementation-plan.md) — the original agreed architecture,
  API, security model, testing strategy, and milestones.
- [Spec](docs/spec.md) — the consolidated specification: user stories, implementation and
  testing decisions, out of scope, residual risks.
- [Git SSH signing interface (spike findings)](docs/git-ssh-signing-interface.md) —
  empirically verified `gpg.ssh.program` behavior that the client implements (sign argv,
  two-step verify protocol, Git's exit-code semantics).

## Quick start

The whole system is three steps: deploy the signer, register its public key with GitLab,
then provision agent VMs.

### 1. Deploy the signer

On a persistent VM, [`deploy/install-server.sh`](deploy/install-server.sh) installs
`git-signer-server` as a hardened systemd service under a dedicated unprivileged
`git-signer` account. It requires the pinned committer identity and the VM allowlist:

```bash
sudo env \
  SIGNER_COMMITTER_NAME='Your Name' \
  SIGNER_COMMITTER_EMAIL='you@example.com' \
  SIGNER_ALLOWLIST='agent-*' \
  deploy/install-server.sh
```

It creates `/var/lib/git-signer` (mode `0700`), generates an ED25519 key if none exists,
installs `/etc/git-signer/git-signer.env` and `git-signer.service`, starts the service, and
prints the public key. See [key lifecycle](docs/key-lifecycle.md) for backup, rotation, and
recovery.

### 2. Register the public key with GitLab

Register the printed public key (`/var/lib/git-signer/signing_key.pub`) on your personal
GitLab account with usage type **Signing only**. The service's landing page shows the key,
its fingerprint, and the same instructions:

```bash
curl -s http://127.0.0.1:8000/        # landing page: public key + GitLab signing steps
```

Full walkthrough, including the verified-email requirement for the Verified badge:
[docs/gitlab-setup.md](docs/gitlab-setup.md).

### 3. Provision an agent VM

Attach the exe.dev peer integration to the VM (or, better, to a tag so every future agent
VM inherits it — see [docs/exe-dev-setup.md](docs/exe-dev-setup.md)), then run one command:

```bash
curl -fsSL https://git-signer.int.exe.xyz/install.sh | sh
```

The signer renders `/install.sh` with the pinned configuration baked in (URL, public key,
committer identity); the script downloads the client binary and
[`deploy/install-client.sh`](deploy/install-client.sh) from the signer's `/v1/client/...`
endpoints and runs it. To provision by hand instead (e.g. a client platform the signer
doesn't serve), export the same values and run the installer from a repo checkout:

```bash
SIGNER_URL=https://git-signer.int.exe.xyz \
SIGNER_PUBLIC_KEY=/var/lib/git-signer/signing_key.pub \
SIGNER_COMMITTER_NAME='Your Name' \
SIGNER_COMMITTER_EMAIL='you@example.com' \
  deploy/install-client.sh
```

[`deploy/install-client.sh`](deploy/install-client.sh) is idempotent and needs only
`sh`, `git`, `ssh-keygen`, `curl`, `awk`, `sed`, and `mktemp` — plus `tar`, `find`,
and `sha256sum` when it downloads a release artifact instead of using a local
binary. No Nix, Docker, or other runtime. It:

1. checks the signer is reachable (`GET /healthz`);
2. cross-checks the pinned key against `GET /v1/public-key` and aborts on mismatch;
3. installs `git-remote-sign` from `GIT_REMOTE_SIGNER_BIN`, or from a checksum-verified
   [release download](#release-artifacts);
4. installs the pinned public key to `$XDG_CONFIG_HOME/git-remote-signer/signing.pub`
   and a matching allowed-signers file to
   `$XDG_CONFIG_HOME/git-remote-signer/allowed_signers` (so local
   `git verify-commit` trusts the pinned key with no manual setup);
5. sets *only* these user-level git keys: `gpg.format`, `gpg.ssh.program`,
   `commit.gpgsign`, `user.signingkey`, `gpg.ssh.allowedSignersFile`, `user.name`,
   `user.email`;
6. persists the client environment variables in
   `$XDG_CONFIG_HOME/git-remote-signer/env` and sources them from `~/.profile`;
7. runs a self-test commit in a throwaway repository — signed through the real signer and
   verified locally with the pinned key, then removed and never pushed.

Re-running it converges to the same state. It writes no private key, token, or signing
credential: only the binary, the public key, the client environment, and the git
configuration. Pass `--skip-selftest` to configure without the self-test.

`POST /v1/sign` requires the platform-verified VM identity, so `SIGNER_URL`
must be the exe.dev peer-integration URL (or a proxy that stamps the identity). Pointing
the self-test at the signer directly fails closed, as intended.

## Try it locally

One command proves the whole loop on a single machine — generate a throwaway key, start
the signer, sign an ordinary `git commit` through `git-remote-sign`, and verify it with
stock Git:

```bash
scripts/demo-local.sh
```

Everything runs in a temporary directory. The script uses `scripts/devproxy` to stand in
for exe.dev's authenticated peer proxy, which in production stamps the verified
`X-Exedev-Source-Vm` identity that `POST /v1/sign` requires.

> **Warning:** `scripts/devproxy` is a **development-only** stand-in for exe.dev
> platform plumbing. It stamps `X-Exedev-Source-Vm` on every request, so it must
> **never** be deployed in front of a production signer — that would make the
> platform-vouched identity forgeable by any client that can reach it.

Expected tail:

```text
==> verifying the commit signature with stock Git
Good "git" signature for demo@example.com with ED25519 key SHA256:...
demo: OK - commit signed through the remote signer and verified locally
```

The same steps by hand (builds into a temporary directory so the repo stays clean;
`SIGNER_PORT` picks a different signer port if `8000` is taken):

```bash
BIN=$(mktemp -d)
go build -o "$BIN" ./cmd/... ./scripts/devproxy

SIGNER_PORT=${SIGNER_PORT:-8000}
ssh-keygen -q -t ed25519 -N '' -C git-signer -f /tmp/demo-signing-key
SIGNER_KEY_PATH=/tmp/demo-signing-key SIGNER_PORT="$SIGNER_PORT" \
  SIGNER_COMMITTER_NAME="Demo Agent" SIGNER_COMMITTER_EMAIL=demo@example.com \
  SIGNER_ALLOWLIST=local-demo-vm "$BIN/git-signer-server" &
"$BIN/devproxy" -listen 127.0.0.1:8080 -upstream "http://127.0.0.1:$SIGNER_PORT" \
  -vm local-demo-vm &
until curl -fsS "http://127.0.0.1:$SIGNER_PORT/readyz" >/dev/null 2>&1; do sleep 0.1; done

export SIGNER_URL=http://127.0.0.1:8080
export SIGNER_PUBLIC_KEY=/tmp/demo-signing-key.pub
printf '%s %s %s\n' demo@example.com $(awk '{print $1, $2}' /tmp/demo-signing-key.pub) \
  >/tmp/demo-allowed-signers

git init demo && cd demo
git config user.name "Demo Agent"
git config user.email demo@example.com
git config gpg.format ssh
git config commit.gpgsign true
git config gpg.ssh.program "$BIN/git-remote-sign"
git config user.signingkey /tmp/demo-signing-key.pub
git config gpg.ssh.allowedSignersFile /tmp/demo-allowed-signers

echo hello > README.md && git add README.md
git commit -m "demo: signed commit"   # signed through the remote signer
git verify-commit HEAD                # verifies with stock ssh-keygen
```

## Running the signer

`git-signer-server` listens on port `8000` (override with `SIGNER_PORT`). It needs the path
to the private signing key (`SIGNER_KEY_PATH`) and the pinned committer identity
(`SIGNER_COMMITTER_NAME`, `SIGNER_COMMITTER_EMAIL`) that every signed commit must name —
all required, with no defaults. The allowlist (`SIGNER_ALLOWLIST`) is also required in
practice: it is fail-closed, so an unset or empty value refuses every request.

```bash
ssh-keygen -t ed25519 -N '' -C git-signer -f /tmp/signing_key
SIGNER_KEY_PATH=/tmp/signing_key \
  SIGNER_COMMITTER_NAME='Dev Eloper' SIGNER_COMMITTER_EMAIL='dev@example.com' \
  SIGNER_ALLOWLIST='agent-*' go run ./cmd/git-signer-server
```

```bash
curl -s http://127.0.0.1:8000/healthz                  # liveness: "ok"
curl -s http://127.0.0.1:8000/readyz                   # readiness (503 until the key loads)
curl -s http://127.0.0.1:8000/v1/public-key            # public signing key
curl -s -H 'X-Exedev-Source-Vm: agent-1' --data-binary @commit-payload \
  http://127.0.0.1:8000/v1/sign                        # raw SSHSIG
```

| Endpoint | Method | Purpose |
|---|---|---|
| `/` | GET | Read-only landing page: public key, fingerprint, GitLab registration steps |
| `/v1/sign` | POST | Sign a commit payload (identity-gated); returns the raw SSHSIG |
| `/v1/public-key` | GET | The public signing key (informational; clients pin it) |
| `/healthz` | GET | Liveness |
| `/readyz` | GET | Readiness — `503` until the signing key is loaded |

`POST /v1/sign` requires the platform-verified source-VM identity
(`X-Exedev-Source-Vm`, set by the exe.dev peer proxy in production) and authorizes it
against `SIGNER_ALLOWLIST`, a comma-separated list of exact VM names and `path.Match` glob
patterns (for example `agent-*,ci-runner`). There is no allow-all default: an unset or
empty allowlist refuses every request (`401` when the identity is missing, `403` when it is
not allowlisted). Per-VM rate limiting returns `429` once a VM exhausts its token bucket;
the sustained rate is `SIGNER_RATE_PER_MIN` (default `60`) with a burst capacity of
`SIGNER_RATE_BURST` (default `10`). Every signing decision emits one structured `slog`
JSON audit line carrying the VM, payload SHA-256, status and duration — never the payload.

`POST /v1/sign` signs commits only. The payload is parsed structurally as a git commit
object (headers with continuation lines, blank-line separator, message body — never
regex-matched) and must name exactly the configured committer
(`SIGNER_COMMITTER_NAME`/`SIGNER_COMMITTER_EMAIL`). Anything else is refused before it
reaches the signing backend, with a distinct status: malformed payload `400`, oversized
payload `413` (limit 1 MiB), payload already carrying a `gpgsig`/`gpgsig-sha256` header
`422`, and a committer other than the pinned identity `409`. The rejection reason is logged
as metadata only (a stable reason code, the payload SHA-256, its size) — never the payload
itself. The author's identity is deliberately not checked: GitLab verifies the committer.

See [docs/architecture.md](docs/architecture.md) for the complete configuration table and
the sign/verify request flows.

## Running the client

`git-remote-sign` implements Git's `gpg.ssh.program` contract. It is configured with two
required environment variables and an optional timeout:

```bash
export SIGNER_URL=https://git-signer.int.exe.xyz   # the signer base URL
export SIGNER_PUBLIC_KEY="$HOME/.config/git-remote-signer/signing.pub"
export SIGNER_TIMEOUT=10s   # optional; default 10s
```

`SIGNER_PUBLIC_KEY` is the *pinned* trusted key: either a literal
authorized_keys line or a path to a file containing one. The key passed by Git as
`-f`/`user.signingkey` must match it, and every signature returned by the server is
verified locally against the pinned key before `<buffer>.sig` is written — atomically, via
a temp file plus rename. Any failure exits non-zero and removes a partial `.sig`, so Git
aborts the commit. There is never an unsigned fallback.

Git also routes verification through the configured program. The operations `verify`,
`find-principals` and `check-novalidate` are delegated verbatim to the real system
`ssh-keygen` — the original arguments (including `-f`, which in verify mode is the
allowed-signers file), stdin/stdout and exit code are preserved — so `git verify-commit`
and `git log --show-signature` work normally with stock OpenSSH. Unknown operations fail
loudly rather than being mishandled.

## Release artifacts

Releases are cut from a tag; `deploy/install-client.sh` downloads and checksum-verifies the
client artifact unless you pass a local binary. For tag `vX.Y.Z` it expects, under
`$GIT_REMOTE_SIGNER_DOWNLOAD_BASE/vX.Y.Z/` (default: this repository's GitHub releases):

```text
git-remote-sign_X.Y.Z_<os>_<arch>.tar.gz   # contains the git-remote-sign binary
checksums.txt                              # sha256sum format
```

where `<os>` is `linux` or `darwin` and `<arch>` is `amd64` or `arm64`. A missing checksum
entry or a mismatch aborts the install; an unverified download is never executed.

**v0.1.0** publishes, for linux/amd64 and linux/arm64:

| Asset | For |
|---|---|
| `git-remote-sign_0.1.0_linux_<arch>.tar.gz` | Agent VMs — consumed by `install-client.sh` |
| `git-remote-sign-linux-<arch>` | Manual client install (`GIT_REMOTE_SIGNER_BIN=...`) |
| `git-signer-server-linux-<arch>` | Manual server install (`SIGNER_SERVER_BIN=...`) |
| `checksums.txt` | SHA-256 checksums for every asset above |

Build them (verified recipe; cross-compiles cleanly, no cgo):

```bash
VERSION=0.1.0
mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -o "dist/git-remote-sign-linux-$arch" ./cmd/git-remote-sign
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -o "dist/git-signer-server-linux-$arch" ./cmd/git-signer-server
done

mkdir -p "dist/v$VERSION"
for arch in amd64 arm64; do
  staged=$(mktemp -d)
  cp "dist/git-remote-sign-linux-$arch" "$staged/git-remote-sign"
  tar -czf "dist/v$VERSION/git-remote-sign_${VERSION}_linux_${arch}.tar.gz" \
    -C "$staged" git-remote-sign
  rm -rf "$staged"
done
cp dist/git-remote-sign-linux-* dist/git-signer-server-linux-* "dist/v$VERSION/"
(cd "dist/v$VERSION" && sha256sum \
  git-remote-sign_${VERSION}_linux_amd64.tar.gz \
  git-remote-sign_${VERSION}_linux_arm64.tar.gz \
  git-remote-sign-linux-amd64 git-remote-sign-linux-arm64 \
  git-signer-server-linux-amd64 git-signer-server-linux-arm64 > checksums.txt)
```

Attach everything in `dist/v$VERSION/` to the `v$VERSION` release. Until the release is
published, install a locally built binary with `GIT_REMOTE_SIGNER_BIN=./git-remote-sign`.

## Status

**Implemented (v0.1.0).** All five milestones are complete:

- **M1 — local proof of concept:** `scripts/demo-local.sh` runs the whole loop on one
  machine.
- **M2 — exe.dev integration:** identity-gated `POST /v1/sign` with the platform-verified
  `X-Exedev-Source-Vm` identity, a fail-closed allowlist, and per-VM rate limiting; setup
  documented in [docs/exe-dev-setup.md](docs/exe-dev-setup.md).
- **M3 — agent provisioning:** `deploy/install-client.sh` takes a fresh VM to
  signing-capable in one idempotent run (verified binary, pinned key, git config,
  reachability + key cross-check, signing self-test).
- **M4 — security and reliability:** structural commit validation, size limits, pinned
  committer, structured audit logging, systemd hardening, and graceful SIGTERM shutdown.
- **M5 — documentation and release:** this documentation set and the v0.1.0 release
  artifacts.

The server's HTTP API (`GET /` landing page, `POST /v1/sign`, `GET /v1/public-key`,
`GET /healthz`, `GET /readyz`) is implemented on the `ssh-keygen` signing backend. The
client implements the whole `gpg.ssh.program` contract: signing (payload forwarding,
pinned-key check, local signature verification, atomic `.sig` write) and verbatim
verification passthrough (`verify`, `find-principals`, `check-novalidate`). Tests cover
three seams: HTTP black box, the signing interface, and a real-Git OS end-to-end test that
asserts `git verify-commit` exit codes (0 valid, 1 unknown signer, 128 corrupt).

**Remaining gate:** the [manual GitLab acceptance checklist](docs/acceptance-checklist.md)
— register the key with a real GitLab account, commit from a real agent VM with the
verified email, push to a disposable repository, and confirm the **Verified** badge. This
is required before production use.

## Security

Read [docs/security.md](docs/security.md) for the threat model and trust boundaries. In
short: the private key lives only on the signer; the platform vouches for the caller
identity; no client ever trusts a fetched key (the public key is pinned); and there is no
unauthenticated fallback. The accepted residual risk is that any authorized VM can request
a signature for an arbitrary commit under the developer's identity — mitigated by the
audit log, protected branches and review, and revocation by removing the key from GitLab.

## License

See [LICENSE](LICENSE).
