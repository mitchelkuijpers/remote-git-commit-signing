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

## Status

🚧 **Spec complete, no code yet.** The [spike](docs/git-ssh-signing-interface.md)
verified Git's `gpg.ssh.program` contract and the [spec](docs/spec.md) is ready.
Next: confirm the test seams, then start Milestone 1 test-first.

## License

See [LICENSE](LICENSE).
