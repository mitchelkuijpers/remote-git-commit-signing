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
- [Signing key lifecycle](docs/key-lifecycle.md) — server deployment, key generation,
  GitLab registration (Signing-only), backup, rotation, and recovery.

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
curl -s http://127.0.0.1:8000/                         # landing page: public key + GitLab signing steps
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

`git-remote-sign` implements the signing half of Git's `gpg.ssh.program` contract. It is
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
exits non-zero and removes a partial `.sig`, so Git aborts the commit. Verification
operations (`verify`, `find-principals`, `check-novalidate`) are delegated to the real
`ssh-keygen` in a later slice and currently fail loudly with `not implemented yet`.

## Deploying the signer

On a persistent systemd VM, [`deploy/install-server.sh`](deploy/install-server.sh) installs
`git-signer-server` as a hardened service under a dedicated `git-signer` account (restart on
failure, journald logs, graceful SIGTERM shutdown that drains in-flight signatures). It
requires the committer identity and the VM allowlist:

```bash
sudo env \
  GIT_SIGNER_COMMITTER_NAME='Your Name' \
  GIT_SIGNER_COMMITTER_EMAIL='you@example.com' \
  GIT_SIGNER_ALLOWLIST='agent-*' \
  deploy/install-server.sh
```

It creates `/var/lib/git-signer` (mode `0700`), generates an ED25519 key if none exists, and
prints the public key to register with GitLab as a **Signing-only** key. See the
[signing key lifecycle](docs/key-lifecycle.md) runbook for configuration, backup, rotation,
recovery, and service operations.

## Status

🚧 **Milestone 1 in progress.** The [spike](docs/git-ssh-signing-interface.md) verified
Git's `gpg.ssh.program` contract, the [spec](docs/spec.md) is ready, and the signer server's
minimal HTTP API (`GET /` landing page, `POST /v1/sign`, `GET /v1/public-key`, `GET /healthz`, `GET /readyz`)
is implemented on top of the `ssh-keygen` signing backend, with VM-identity authorization
(identity header, allowlist, per-VM rate limit, audit log) on `POST /v1/sign`. The
`git-remote-sign` client now implements the signing half of the `gpg.ssh.program` contract
(payload forwarding, pinned-key check, local signature verification, atomic `.sig` write).
Next: verify-path delegation and the end-to-end `git commit` flow.

## License

See [LICENSE](LICENSE).
