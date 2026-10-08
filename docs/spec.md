# Spec: Remote Git Commit Signing for exe.dev Agents

> **Status:** Implemented (v0.1.0). Seams confirmed and covered by tests; the exe.dev
> peer integration is treated as transparent plumbing (not under test). Published to the
> issue tracker as
> [issue #1](https://github.com/mitchelkuijpers/remote-git-commit-signing/issues/1)
> — this file is canonical. What is built is described in
> [architecture.md](architecture.md); the remaining gate is the
> [manual GitLab acceptance checklist](acceptance-checklist.md).
> Companion documents: [implementation plan](implementation-plan.md),
> [Git SSH signing interface — spike findings](git-ssh-signing-interface.md).

## Problem Statement

The developer runs many short-lived coding-agent VMs on exe.dev. Commits made by those
agents should appear in GitLab as **Verified** commits from the developer's personal
account, which requires SSH commit signing with a key registered on that account.

Today there is no safe way to do this: copying the developer's private signing key onto
every ephemeral VM spreads the key across short-lived, potentially compromised machines,
and creating a new keypair per VM means registering a new key with GitLab every time. Both
options are key-management liabilities. The developer wants ordinary `git commit` on any
agent VM to produce a validly signed commit, without any signing key ever existing on
agent VMs and without manually managed credentials between agents and the signing service.

## Solution

A two-part system:

1. **`git-signer-server`** — a persistent HTTP service on a dedicated exe.dev VM that holds
   the single ED25519 private signing key, authenticates callers through exe.dev's
   VM-to-VM peer integration, validates and signs commit payloads, and returns standard
   OpenSSH SSHSIG signatures. Its public web page also serves the public key plus
   copy-paste instructions for registering it with GitLab. Listens on **port 8000**.

2. **`git-remote-sign`** — a small client binary on each agent VM that plugs into Git's
   `gpg.ssh.program` hook. On signing requests it forwards the exact payload bytes to the
   server, cryptographically validates the returned signature locally, and writes it where
   Git expects it. On verification requests it delegates to the real `ssh-keygen`.
   Installed by a single idempotent script; unaffected by VM teardown otherwise.

The private key exists only on the signer VM. Agent provisioning is one command and leaves
nothing to clean up on VM destruction. Signing failures abort the commit — no silent
unsigned fallbacks.

## User Stories

### Signing core

1. As the developer, I want a single persistent signing key, so that I only register one key with GitLab.
2. As the developer, I want the private key to exist only on the signer VM, so that ephemeral VMs never handle key material.
3. As an agent, I want a normal `git commit` to automatically produce an SSH-signed commit, so that signing needs no extra commands or changes to my workflow.
4. As the developer, I want signatures to be standard OpenSSH SSHSIG in the `git` namespace, so that they verify with stock `ssh-keygen` and are recognized by GitLab.
5. As an agent, I want the exact bytes Git hands over to be signed, so that the resulting commit verifies bit-for-bit.
6. As an agent, I want commits to fail with a clear error when signing fails, so that I never accidentally push unsigned or corruptly-signed commits.
7. As the developer, I want the client to cryptographically validate the returned signature before accepting it, so that a broken or hostile server response cannot produce a permanently corrupt commit (Git embeds garbage `.sig` content without complaint — verified in the spike).
8. As the developer, I want Git verification operations (`git log --show-signature`, `git verify-commit`) to work normally on agent VMs, so that I can locally inspect signed history.

### Identity, authentication, authorization

9. As the developer, I want signing requests authenticated by exe.dev's VM-to-VM peer integration, so that only attached VMs can reach the signer and I manage no tokens.
10. As the developer, I want authorization based on the platform-verified source-VM identity (never on an unauthenticated header), so that a random internet client cannot impersonate an agent.
11. As the developer, I want an allowlist of permitted VM names/patterns on the server, so that a second authorization layer exists beyond tag-based attachment.
12. As the developer, I want the server to enforce my configured committer name and verified email, so that no commit is signed under a different identity.
13. As the developer, I want the server to reject structurally invalid or oversized commit payloads, or ones with pre-existing signatures, so that the signer can't be abused for arbitrary data signing.
14. As the developer, I want rate limiting on signing requests, so that a runaway agent can't hammer the service.
15. As the developer, I want every signing decision logged with the requesting VM identity, so that I have an audit trail of who signed what.

### Server operation

16. As the operator, I want a systemd unit with hardening and auto-restart, so that the service runs unprivileged and survives crashes.
17. As the operator, I want a simple web page at the service address showing the public key and GitLab registration instructions, so that teammates can copy-paste setup steps without reading a wiki.
18. As the operator, I want `/healthz` and `/readyz` endpoints, so that I can monitor liveness and readiness without exposing key material.
19. As the operator, I want structured (`slog`) logs visible via `journalctl`, so that operations integrate with standard tooling.
20. As the operator, I want documented key generation, backup, rotation, and recovery procedures, so that the single key is not a single point of failure.

### Agent provisioning

21. As the developer, I want a single idempotent install command for fresh agent VMs, so that spinning up a new VM takes seconds and is safe to re-run.
22. As the developer, I want installing to require no Nix/Docker/runtime and no unverified downloads, so that it works on minimal agent images safely.
23. As the developer, I want the trusted public key pinned/validated on the client (never blindly fetched), so that a hostile or misconfigured server cannot substitute keys.
24. As the developer, I want the install to run a harmless self-test signing round-trip, so that misconfiguration is caught before real work starts — without pushing anything anywhere.
25. As the developer, I want client config to be just two environment variables plus git config, so that the setup is easy to eyeball and debug.

### Acceptance

26. As the developer, I want GitLab to display commits from agent VMs as **Verified**, so that the whole system delivers its purpose.
27. As the developer, I want concurrent commits from many agent VMs to work, so that the service scales to my actual usage.
28. As the developer, I want destroying an agent VM to require zero signing-related cleanup, so that ephemerality stays cheap.

## Implementation Decisions

1. **Two Go binaries built from one module**: `git-signer-server` (server) and
   `git-remote-sign` (client), sharing internal packages. Standard library first
   (`net/http`, `log/slog`, `os/exec`); no framework, ORM, or service dependencies.

2. **Server listens on port 8000** (confirmed by user). In production exposure is
   controlled by exe.dev's peer integration, not by ad-hoc token checks in the app;
   loopback-only deployments remain a supported configuration choice.

3. **Public landing page**: the server serves a minimal HTML page at `/` showing the
   SSH public key and step-by-step instructions for registering it as a *Signing* key
   in GitLab (confirmed by user). Non-signing, read-only content; renders nothing sensitive.

4. **Git client contract (verified by spike — see
   [spike findings](git-ssh-signing-interface.md)):**
   - Sign invocation is positional: `-Y sign -n git -f <user.signingkey> <buffer>`. The
     operation is determined by argument position, never by substring matching (a glob
     bug bit us during the spike: `find-principals` and a temp *path* both match `*sign*`).
   - Signature goes to `<buffer>.sig`; the buffer is in `/tmp`, so writes are atomic and
     failures clean up partial files.
   - Verify is a **two-call protocol**: `find-principals` then `verify -I <principal>`
     with the payload on **stdin** (fallback: `check-novalidate`). All three operations are
     verbatim passthrough (argv, stdin, exit code) to the real `ssh-keygen`.
   - **Git does not validate the signature at commit time** — a garbage `.sig` gets
     permanently embedded. Therefore the client must validate the returned signature
     cryptographically against the payload and the pinned public key **before** writing
     `.sig` and exiting 0.

5. **Signing backend behind a Go interface**, initial implementation shells out to
   `ssh-keygen -Y sign` (no shell, timeouts, unique temp files, input size limits). A
   future hardware/HSM backend can replace it without touching HTTP or validation code.

6. **Server-side commit validation** before signing: structural parse of the Git commit
   payload (not regex-only; multilineheader-aware — the spike captured the exact payload
   shape: `tree`/`parent`/`author`/`committer`/blank/message), enforced committer name +
   email via `SIGNER_COMMITTER_NAME`/`SIGNER_COMMITTER_EMAIL`, size limits, `git`
   namespace only, no pre-existing `gpgsig` header. Commits only for MVP; signed tags later.

7. **Authentication = exe.dev peer integration** providing the verified
   `X-Exedev-Source-Vm` identity. The server rejects identity-less requests outright and
   has **no** unauthenticated fallback. Mock-header handling exists only in tests.
   Authorization adds a server-side allowlist of VM names/patterns on top of tag-based
   attachment.

8. **Repository-level authorization is out of scope** and explicitly not claimed: the
   signing payload contains no trustworthy repo identity, so enforcement is VM-level
   (documented residual risk).

9. **Client configuration**: `GIT_REMOTE_SIGNER_URL` + `GIT_REMOTE_SIGNER_PUBLIC_KEY`
   (pinned key), configurable timeout with sane default, HTTP timeouts, response size
   limits, no retry storms, no payload logging, and **never** an unsigned fallback.

10. **Provisioning**: one idempotent `install-client.sh` script — installs binary,
    installs/validates pinned public key, sets git config (`gpg.format=ssh`,
    `gpg.ssh.program`, `commit.gpgsign`, `user.signingkey`, name/email), checks server
    reachability, runs a harmless local signing self-test. No Nix/Docker required; any
    downloaded artifacts are checksum-verified. No dotfiles mechanism exists yet, so none
    is integrated; config is written in a dotfiles-forward-compatible way.

11. **API**: versioned minimal HTTP — `POST /v1/sign` (octet-stream in, raw sshsig PEM
    out, distinct status codes), `GET /v1/public-key` (informational only; clients pin),
    `GET /healthz`, `GET /readyz` (ready only when a key is loaded, without exposing it).

12. **Observability**: `slog` JSON logs with per-request fields (`event`, `vm`, `status`,
    `duration_ms`, request id, payload **hash** — never the payload itself) plus in-process
    counters for signed/rejected/failed. No metrics infrastructure in MVP.

13. **Test-first workflow**: the repo's `tdd` skill drives implementation —
    red → green → refactor per ticket (confirmed by user).

14. **Developer identity** (`SIGNER_COMMITTER_NAME`/`EMAIL`, git user/email on agents) is
    deployment configuration, supplied at provisioning time; the codebase treats it as a
    required-with-no-default setting.

## Testing Decisions

**What makes a good test here:** assert on externally observable behavior at the highest
seam that can realistically fake reality — HTTP semantics at the API boundary, real Git
behavior at the OS boundary — never on internal call graphs.

**Confirmed seams (3 total):**

1. **HTTP API seam** — server tested as a black box over real HTTP using a test key:
   request validation, authz policies, payload validation, status codes, size limits,
   rate limiting, concurrency. The exe.dev peer integration is **treated as transparent
   plumbing** (confirmed by user): no exe.dev-specific test harness, fake proxies, or
   header-spoofing tests. Requests in tests simply carry the identity the same way the
   transparent platform does.
2. **Signing backend interface seam** — the Go interface around signing allows fast unit
   tests of server logic without subprocess overhead; the real `ssh-keygen` backend is
   itself verified through seam 1 and seam 3.
3. **OS/Git seam (highest, client-end)** — the client is validated by driving the real
   `git` binary end-to-end: temp repo, real keypair, real `git commit` with
   `git-remote-sign` as `gpg.ssh.program` against a locally running server, then strict
   `git verify-commit` assertions. Spike finding: assert on `git verify-commit` exit
   codes — `git log --show-signature` is exit-code lenient.

**Coverage commitments beyond happy path:** signer-failure ⇒ commit aborts (Git exit 128);
invalid signature from server ⇒ commit aborts before `.sig` is written; unauthorized
caller ⇒ 403 and no signature; oversized/invalid payload ⇒ 4xx; service unreachable ⇒
commit aborts; signing timeout ⇒ commit aborts; concurrent commits all verify; garbage
`.sig` never becomes a commit (regression test for the spike's critical finding).

**Acceptance gate (not fully automatable):**
- **GitLab Verified acceptance test**: register key with GitLab, commit from a real agent
  VM with the verified email, push to a disposable repo, confirm **Verified** badge.
  Required before production use. (No exe.dev-specific acceptance test: the peer
  integration is platform plumbing, not our code — see seam 1.)

## Out of Scope

- Signed tags (regular commit signing only in MVP).
- Repository-level authorization (impossible to do trustworthily from payload alone; future
  enhancement needs an independently verifiable mechanism).
- Databases, queues, web dashboards, metrics infrastructure, external secret managers, K8s.
- The author's identity (GitLab verifies committer identity only).
- GPG-format signing, non-ED25519 keys, macOS clients (structure allows; not built).
- Git push machinery itself — signing and pushing stay separate concerns; agents' GitLab
  push credentials are managed outside this project.

## Further Notes

- **Residual risk to document (accepted):** any authorized VM can request signatures for
  arbitrary commits under the developer's identity. Mitigations: audit log + protected
  branch reviews + revocation by GitLab key removal. The signer proves *an authorized VM
  asked*, not *the developer approved*.
- GitLab verification requires the **committer email** to match a verified email on the
  account owning the key — hence server-enforced committer validation (decision 6/14).
- The GitLab public key is registered **Signing** scope only.
- Spike evidence and exact argv transcripts are preserved in
  [git-ssh-signing-interface.md](git-ssh-signing-interface.md).
