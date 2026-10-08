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
SIGNER_KEY_PATH=/tmp/signing_key go run ./cmd/git-signer-server
```

```bash
curl -s http://127.0.0.1:8000/healthz                  # liveness
curl -s http://127.0.0.1:8000/readyz                   # readiness (503 until the key loads)
curl -s http://127.0.0.1:8000/v1/public-key            # public signing key
curl -s --data-binary @commit-payload \
  http://127.0.0.1:8000/v1/sign                        # raw SSHSIG PEM
```

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

## Status

🚧 **Milestone 1 in progress.** The [spike](docs/git-ssh-signing-interface.md) verified
Git's `gpg.ssh.program` contract, the [spec](docs/spec.md) is ready, and the signer server's
minimal HTTP API (`POST /v1/sign`, `GET /v1/public-key`, `GET /healthz`, `GET /readyz`)
is implemented on top of the `ssh-keygen` signing backend. The `git-remote-sign` client
now implements the signing half of the `gpg.ssh.program` contract (payload forwarding,
pinned-key check, local signature verification, atomic `.sig` write). Next: verify-path
delegation and the end-to-end `git commit` flow.

## License

See [LICENSE](LICENSE).
