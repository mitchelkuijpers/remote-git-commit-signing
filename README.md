# remote-git-commit-signing

Remote Git commit signing for [exe.dev](https://exe.dev) agents.

A lightweight, centralized SSH commit-signing service that lets dozens of short-lived
agent VMs produce Git commits signed with a single signing key — **without ever putting
the private key on an agent VM**.

## Why

Ephemeral agent VMs are created and destroyed constantly. Distributing a private GitLab
signing key to each one creates a key-management problem. This project keeps the private
key on one persistent signer VM and exposes a small, authenticated signing API to agents.

- **One** SSH signing key, registered once with GitLab.
- **Zero** private signing keys on agent VMs.
- **Zero** manually managed credentials between agents and the signer.
- Signing happens automatically with an ordinary `git commit`.
- Minimal per-VM configuration.

## How it works

Two small Go programs:

| Binary | Runs on | Role |
|---|---|---|
| `git-signer-server` | Persistent signer VM | Holds the private key, validates requests, returns SSHSIG signatures |
| `git-remote-sign` | Ephemeral agent VMs | Implements Git's `gpg.ssh.program` interface and calls the signer |

Agent VMs reach the signer through exe.dev's authenticated VM-to-VM peer integration, which
provides a platform-verified caller identity. The private key never leaves the signer.

See [`docs/implementation-plan.md`](docs/implementation-plan.md) for the full architecture,
API, security model, and milestones.

## Documentation

- [Implementation plan](docs/implementation-plan.md) — architecture, API, security model,
  testing strategy, and milestones. **Status: proposed, not yet implemented.**
- [Spec](docs/spec.md) — the consolidated specification: user stories, implementation
  and testing decisions, out of scope, residual risks. Written after the interface spike.
- [Git SSH signing interface (spike findings)](docs/git-ssh-signing-interface.md) —
  empirically verified `gpg.ssh.program` behavior that the client must implement
  (sign argv, two-step verify protocol, git's exit-code semantics).

Additional docs (`architecture`, `exe-dev-setup`, `gitlab-setup`, `security`,
`troubleshooting`) will be added as implementation proceeds.

## Running the signer locally

`git-signer-server` listens on port `8000` (override with `SIGNER_PORT`) and needs one
required setting, the path to the private signing key (`SIGNER_KEY_PATH`, no default):

```bash
ssh-keygen -t ed25519 -N '' -C git-signer -f /tmp/signing_key
SIGNER_KEY_PATH=/tmp/signing_key SIGNER_ALLOWLIST='agent-*' go run ./cmd/git-signer-server
```

```bash
curl -s http://127.0.0.1:8000/healthz                  # liveness
curl -s http://127.0.0.1:8000/readyz                   # readiness (503 until the key loads)
curl -s http://127.0.0.1:8000/v1/public-key            # public signing key
curl -s -H 'X-Exedev-Source-Vm: agent-1' --data-binary @commit-payload \
  http://127.0.0.1:8000/v1/sign                        # raw SSHSIG PEM
```

`POST /v1/sign` requires the platform-verified source-VM identity
(`X-Exedev-Source-Vm`, set by the exe.dev peer proxy in production) and authorizes it
against `SIGNER_ALLOWLIST`, a comma-separated list of exact VM names and `path.Match`
glob patterns (for example `agent-*,ci-runner`). There is no allow-all default: an unset
or empty allowlist refuses every request (401 when the identity is missing, 403 when it
is not allowlisted). Per-VM rate limiting returns 429 once a VM exhausts its token
bucket; the sustained rate is `SIGNER_RATE_PER_MIN` (default 60) with a burst capacity of
`SIGNER_RATE_BURST` (default 10). Every signing decision emits one structured `slog`
JSON audit line carrying the VM, payload SHA-256, status and duration — never the payload.

The API also accepts the committer identity settings `SIGNER_COMMITTER_NAME` and
`SIGNER_COMMITTER_EMAIL`; they are parsed but not yet enforced (commit validation is a
later slice).

## Running the client

`git-remote-sign` implements Git's `gpg.ssh.program` contract. It is
configured with two required environment variables and an optional timeout:

```bash
export GIT_REMOTE_SIGNER_URL=http://127.0.0.1:8000
export GIT_REMOTE_SIGNER_PUBLIC_KEY="$HOME/.config/git-remote-signer/signing.pub"
export GIT_REMOTE_SIGN_TIMEOUT=10s   # optional; default 10s
```

`GIT_REMOTE_SIGNER_PUBLIC_KEY` is the *pinned* trusted key: either a literal
authorized_keys line or a path to a file containing one. The key passed by Git as
`-f`/`user.signingkey` must match it, and every signature returned by the server is
verified locally against the pinned key before `<buffer>.sig` is written. Any failure
exits non-zero and removes a partial `.sig`, so Git aborts the commit.

Git also routes verification through the configured program. The operations
`verify`, `find-principals` and `check-novalidate` are delegated verbatim to the real
system `ssh-keygen` — the original arguments (including `-f`, which in verify mode is
the allowed-signers file), stdin/stdout and exit code are preserved — so
`git verify-commit` and `git log --show-signature` work normally with stock OpenSSH.
Unknown operations fail loudly rather than being mishandled.

## Milestone 1 local demo

One command proves the whole loop on a single machine — generate a throwaway key,
start the signer, sign an ordinary `git commit` through `git-remote-sign`, and verify
it with stock Git:

```bash
scripts/demo-local.sh
```

Everything runs in a temporary directory. The script uses `scripts/devproxy` to stand
in for exe.dev's authenticated peer proxy, which in production stamps the verified
`X-Exedev-Source-Vm` identity that `POST /v1/sign` requires; the shim does exactly that
locally. Expected tail:

```text
==> verifying the commit signature with stock Git
Good "git" signature for demo@example.com with ED25519 key SHA256:...
demo: OK - commit signed through the remote signer and verified locally
```

The same steps by hand:

```bash
go build -o bin ./cmd/git-signer-server ./cmd/git-remote-sign ./scripts/devproxy

ssh-keygen -q -t ed25519 -N '' -C git-signer -f /tmp/demo-signing-key
SIGNER_KEY_PATH=/tmp/demo-signing-key SIGNER_PORT=8000 \
  SIGNER_COMMITTER_NAME="Demo Agent" SIGNER_COMMITTER_EMAIL=demo@example.com \
  SIGNER_ALLOWLIST=local-demo-vm ./bin/git-signer-server &
./bin/devproxy -listen 127.0.0.1:8080 -upstream http://127.0.0.1:8000 -vm local-demo-vm &

export GIT_REMOTE_SIGNER_URL=http://127.0.0.1:8080
export GIT_REMOTE_SIGNER_PUBLIC_KEY=/tmp/demo-signing-key.pub
printf '%s %s %s\n' demo@example.com $(awk '{print $1, $2}' /tmp/demo-signing-key.pub) \
  >/tmp/demo-allowed-signers

git init demo && cd demo
git config user.name "Demo Agent"
git config user.email demo@example.com
git config gpg.format ssh
git config commit.gpgsign true
git config gpg.ssh.program "$OLDPWD/bin/git-remote-sign"
git config user.signingkey /tmp/demo-signing-key.pub
git config gpg.ssh.allowedSignersFile /tmp/demo-allowed-signers

echo hello > README.md && git add README.md
git commit -m "demo: signed commit"   # signed through the remote signer
git verify-commit HEAD                # verifies with stock ssh-keygen
```

## Status

✅ **Milestone 1 complete (localhost proof of concept).** The
[spike](docs/git-ssh-signing-interface.md) verified Git's `gpg.ssh.program` contract,
the [spec](docs/spec.md) is ready, and the signer server's HTTP API (`POST /v1/sign`,
`GET /v1/public-key`, `GET /healthz`, `GET /readyz`) is implemented on top of the
`ssh-keygen` signing backend, with VM-identity authorization (identity header,
allowlist, per-VM rate limit, audit log) on `POST /v1/sign`. The `git-remote-sign`
client implements the whole `gpg.ssh.program` contract: signing (payload forwarding,
pinned-key check, local signature verification, atomic `.sig` write) and verbatim
verification passthrough (`verify`, `find-principals`, `check-novalidate`) to the
system `ssh-keygen`. A real-Git end-to-end test commits in a temporary repository
against a locally running server and asserts `git verify-commit` exit codes (0 valid,
1 unknown signer, 128 corrupt). Run the whole loop with
[`scripts/demo-local.sh`](scripts/demo-local.sh).

## License

See [LICENSE](LICENSE).
