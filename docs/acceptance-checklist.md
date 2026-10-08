# Acceptance checklist — GitLab Verified (manual gate)

> **Status: NOT YET RUN.** This is the ticket #12 gate: a manual, real-world
> end-to-end that involves a real GitLab account. It **blocks production use** of
> this project until it passes. Do not mark it complete without recorded evidence.

Everything else is covered by automated tests (HTTP black box, signing interface,
and a real-Git OS end-to-end). What cannot be automated is GitLab's own verifier and
a real agent VM behind the exe.dev peer integration — that is what this checklist
exercises. Spec #1 can be closed once every box below is checked with evidence.

## Preconditions

- [ ] Signer VM deployed ([key-lifecycle.md](key-lifecycle.md)) and reachable at the
      peer-integration URL.
- [ ] A disposable GitLab project available (throwaway; safe to delete afterwards).
- [ ] A fresh agent VM available, not yet provisioned.
- [ ] `git`, `ssh-keygen`, and `curl` present on the agent VM.

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

Evidence: <!-- TODO: fingerprint, GitLab key ID/URL, screenshot -->

Outcome: <!-- TODO: pass/fail + notes -->

### 2. Confirm the committer email is verified

Action: ensure the email configured as `SIGNER_COMMITTER_EMAIL` is listed and
verified under the GitLab account's *Emails* (or set it to one that is; a `409` from
the signer means the payload committer does not match the pinned identity).

Expected: `SIGNER_COMMITTER_EMAIL` is a verified email on the account that owns the
registered key.

Evidence: <!-- TODO: email + verification state -->

Outcome: <!-- TODO: pass/fail + notes -->

### 3. Provision a real agent VM with the installer

Action: attach the peer integration to the VM (a `agent` tag, or a direct
attachment; see [exe-dev-setup.md](exe-dev-setup.md)), then run:

```bash
GIT_REMOTE_SIGNER_URL=http://git-signer.int.exe.xyz \
GIT_REMOTE_SIGNER_PUBLIC_KEY=/var/lib/git-signer/signing_key.pub \
GIT_SIGNER_COMMITTER_NAME='<name>' \
GIT_SIGNER_COMMITTER_EMAIL='<verified email>' \
  deploy/install-client.sh
```

Expected: the installer reaches the signer, cross-checks the pinned key, installs
the client, and reports `self-test passed` (commit signed through the signer and
verified locally with the pinned key). No private key is written to the VM.

Evidence: <!-- TODO: installer output tail, VM name/tag -->

Outcome: <!-- TODO: pass/fail + notes -->

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

Evidence: <!-- TODO: commit hash, signature output -->

Outcome: <!-- TODO: pass/fail + notes -->

### 5. Push to a disposable repository

Action: add the disposable project as a remote and push the commit. (Push
credentials are managed outside this project — signing and pushing are separate
concerns.)

Expected: the push succeeds.

Evidence: <!-- TODO: remote URL, pushed commit hash -->

Outcome: <!-- TODO: pass/fail + notes -->

### 6. Confirm GitLab shows the commit as Verified

Action: open the commit in GitLab and read the badge next to the committer.

Expected: the commit displays as **Verified**.

Evidence: <!-- TODO: commit URL (and screenshot) showing the Verified badge -->

Outcome: <!-- TODO: pass/fail + notes -->

## Sign-off

- [ ] All steps pass with evidence recorded above.
- [ ] Spec #1 can be closed.

| Field | Value |
| --- | --- |
| Date run | <!-- TODO --> |
| Operator | <!-- TODO --> |
| Signer VM / key fingerprint | <!-- TODO --> |
| Agent VM | <!-- TODO --> |
| Disposable project | <!-- TODO --> |
| Overall result | <!-- TODO: PASS / FAIL --> |

If any step fails, use [troubleshooting.md](troubleshooting.md) and record the
observed failure before re-running. A commit that pushes but shows no **Verified**
badge is covered by [gitlab-setup.md](gitlab-setup.md) and troubleshooting.
