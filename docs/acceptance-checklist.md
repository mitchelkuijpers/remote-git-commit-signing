# Acceptance checklist — GitLab Verified (manual gate)

> **Status: PASSED 2026-10-10.** This is the ticket #12 gate: a manual, real-world
> end-to-end that involves a real GitLab account. Every step below passed with
> evidence recorded inline; see *Sign-off* at the bottom. GitLab URLs and
> screenshots are withheld from the repo by operator choice; the badge was
> confirmed by the operator on the pushed commit.

## Agent pre-flight (2026-10-09)

An agent attempted the run and verified everything that does not require human
accounts. None of the manual boxes below are checked by this.

- Release assets: locally built set verified against `checksums.txt`
  (`sha256sum -c` OK for all 6 files). **Assets are not yet attached to the
  `v0.1.0` GitHub release** — the exe.dev GitHub proxy only proxies
  `/repos/OWNER/REPO/...` API paths, not the uploads endpoint, so the upload
  needs a machine with direct GitHub access:
  `gh release upload v0.1.0 <dir>/*` with the verified assets from the build
  machine. No longer a gate: the signer serves the client itself (see
  preconditions).
- Test suite: `go test ./...` green (`cmd/git-signer-server`, `deploy`,
  `internal/client`, `internal/server`, `internal/signing`).
- Software e2e: `scripts/demo-local.sh` passes — commit signed through
  `git-remote-sign` → signer server (via the platform-identity shim) and
  verified by stock Git: `Good "git" signature for demo@example.com`.
- exe.dev peer integration: not reachable from the probing VM
  (`integration not found or not attached`), and exe.dev CLI access requires one
  interactive `ssh exe.dev` key registration by the human.

Remaining: the human-gated steps below.

Everything else is covered by automated tests (HTTP black box, signing interface,
and a real-Git OS end-to-end). What cannot be automated is GitLab's own verifier and
a real agent VM behind the exe.dev peer integration — that is what this checklist
exercises. Spec #1 can be closed once every box below is checked with evidence.

## Preconditions

- [x] Signer VM deployed ([key-lifecycle.md](key-lifecycle.md)) and reachable at the
      peer-integration URL. The client binary comes from the signer's own
      `/v1/client/...` endpoints, so the GitHub release assets are optional
      (archival parity; see *Agent pre-flight* above for the blocked upload).
- [x] A disposable GitLab project available (throwaway; safe to delete afterwards).
- [x] A fresh agent VM available, not yet provisioned.
- [x] `git`, `ssh-keygen`, and `curl` present on the agent VM.

## Steps

### 1. Register the public signing key with GitLab (Signing-only)

Action:

1. Read the public key from the signer: `cat /var/lib/git-signer/signing_key.pub`,
   or open the landing page `curl -s <signer-url>/` and copy the key shown there.
2. Open <https://gitlab.com/-/user_settings/ssh_keys> (or the self-managed
   equivalent), paste the whole line, set **Usage type** to **Signing**, give it a
   title, and add it.

Expected: the key appears under *SSH Keys* with usage **Signing** (not
*Authentication & Signing*), and its fingerprint matches
`ssh-keygen -lf /var/lib/git-signer/signing_key.pub`.

Evidence: key fingerprint `SHA256:XD2RQMyHKVp1dUi2O/dzIzLrJSOQSvd8nusQcn7FLSM`,
served at `/v1/public-key` and matching the on-disk key. Registered by the
operator with usage **Signing** only; GitLab key ID/URL withheld from the repo.

Outcome: pass (2026-10-10).

### 2. Confirm the committer email is verified

Action: ensure the email configured as `SIGNER_COMMITTER_EMAIL` is listed and
verified under the GitLab account's *Emails* (or set it to one that is; a `409` from
the signer means the payload committer does not match the pinned identity).

Expected: `SIGNER_COMMITTER_EMAIL` is a verified email on the account that owns the
registered key.

Evidence: the pinned `SIGNER_COMMITTER_EMAIL` (set in the signer env file; not
restated here) is a verified email on the account — proven transitively: GitLab
would not have shown the pushed commit as Verified otherwise.

Outcome: pass (2026-10-10).

### 3. Provision a real agent VM with the installer

Action: attach the peer integration to the VM (an `agent` tag, or a direct
attachment; see [exe-dev-setup.md](exe-dev-setup.md)), then run the signer-served
bootstrap:

```bash
curl -fsSL https://git-signer.int.exe.xyz/install.sh | sh
```

Expected: the installer reaches the signer, cross-checks the pinned key, installs
the client, and reports `self-test passed` (commit signed through the signer and
verified locally with the pinned key). No private key is written to the VM.

Evidence: agent VM `brent` (peer integration attached). Bootstrap output tail:
key cross-check passed, client installed, `self-test passed: commit signed
through the signer and verified locally` — full output in the #12 issue
comments. One caveat surfaced here: shells opened *before* the install lack the
client env (sourced via `.profile`; `. ~/.config/git-remote-signer/env` fixes
existing shells). No private key material on the VM.

Outcome: pass (2026-10-10).

### 4. Commit with the verified committer email

Action: in a fresh repository on the agent VM:

```bash
git init demo && cd demo
git config user.name '<name>'
git config user.email '<verified email>'    # already set by the installer
echo hello > README.md && git add README.md
git commit -m "acceptance: remote-signed commit"
git verify-commit HEAD
```

Expected: the commit succeeds, and `git verify-commit HEAD` exits `0` with
`Good "git" signature for <verified email>`. The commit carries a `gpgsig` header.

Evidence: commit `919d989ded73555082260e412ef0013aaf0f954e` ("test: remote-signed
commit") in the agent VM's working repo. `git verify-commit HEAD` exited 0 with
`Good "git" signature for <pinned email> with ED25519 key
SHA256:XD2RQMyHKVp1dUi2O/dzIzLrJSOQSvd8nusQcn7FLSM`; `git log --show-signature -1`
agrees. Commit carries the `gpgsig` header.

Outcome: pass (2026-10-10).

### 5. Push to a disposable repository

Action: add the disposable project as a remote and push the commit. (Push
credentials are managed outside this project — signing and pushing are separate
concerns.)

Expected: the push succeeds.

Evidence: pushed commit `919d989ded73555082260e412ef0013aaf0f954e` to the
disposable GitLab project (remote URL withheld from the repo); push succeeded.

Outcome: pass (2026-10-10).

### 6. Confirm GitLab shows the commit as Verified

Action: open the commit in GitLab and read the badge next to the committer.

Expected: the commit displays as **Verified**.

Evidence: operator opened the pushed commit in GitLab and confirmed the
**Verified** badge (2026-10-10). Commit URL/screenshot withheld from the repo.

Outcome: pass (2026-10-10).

## Sign-off

- [x] All steps pass with evidence recorded above.
- [x] Spec #1 can be closed.

| Field | Value |
| --- | --- |
| Date run | 2026-10-10 (agent pre-flight 2026-10-09) |
| Operator | repository owner (GitLab steps), agent-assisted for VM/deploy steps |
| Signer VM / key fingerprint | exe.dev signer VM; `SHA256:XD2RQMyHKVp1dUi2O/dzIzLrJSOQSvd8nusQcn7FLSM` (ED25519) |
| Agent VM | `brent` (exe.dev, peer integration attached) |
| Disposable project | GitLab throwaway project (URL withheld) |
| Overall result | **PASS** |

If any step fails, use [troubleshooting.md](troubleshooting.md) and record the
observed failure before re-running. A commit that pushes but shows no **Verified**
badge is covered by [gitlab-setup.md](gitlab-setup.md) and troubleshooting.
