# GitLab setup

Register the signer's public key with GitLab so commits signed by agent VMs show
as **Verified**, and keep the identity settings consistent with what GitLab
checks.

The key is registered **once**, on the developer's personal GitLab account. It is
never copied to an agent VM.

## Where the public key comes from

Any of these gives you the same line (`ssh-ed25519 AAAA... git-signer ...`):

```bash
cat /var/lib/git-signer/signing_key.pub     # on the signer VM
curl -s https://git-signer.int.exe.xyz/      # landing page: key + fingerprint + steps
ssh-keygen -lf /var/lib/git-signer/signing_key.pub   # fingerprint, to verify you have the right key
```

The landing page served at `GET /` on the signer exists precisely so teammates can
copy the key and the steps without reading a wiki. It renders only the public key
and its SHA256 fingerprint — never the key path or any configuration.

## Register the key (Signing-only)

1. Open <https://gitlab.com/-/user_settings/ssh_keys>. On a self-managed instance,
   use the equivalent page (*Preferences → SSH Keys*, or the avatar menu →
   *Edit profile*).
2. Paste the **entire** public key line, including the `git-signer ...` comment.
3. Set **Usage type** to **Signing**. Do **not** choose *Authentication* or
   *Authentication & Signing*.
4. Give it a recognizable title (for example `remote-git-commit-signing`), then
   click **Add key**.

That is the only GitLab operation this project needs. Nothing is added as a Deploy
key or a project key.

### Why Signing-only

A Signing-only key can validate commit signatures but grants **no** shell, clone,
or push access. If it ever leaked, it could be used to forge commit signatures — but
it could not log in to GitLab or touch any repository. Enabling *Authentication* on
the same key would turn it into a full SSH login credential, which is exactly the
blast radius this project is designed to avoid.

## The committer email must be verified

GitLab shows a commit as **Verified** only when *both* hold:

1. the embedded signature is valid and made by a key registered on the account; and
2. the commit's **committer email** matches an email **verified** on that account.

GitLab verifies the committer, not the author. Therefore the server pins the
committer identity in `SIGNER_COMMITTER_NAME` / `SIGNER_COMMITTER_EMAIL` and refuses
to sign any payload naming a different committer (`409`), and the client installer
writes the same identity to `git config user.name` / `user.email`.

Keep these in sync:

| Setting | Where |
| --- | --- |
| `SIGNER_COMMITTER_NAME` / `SIGNER_COMMITTER_EMAIL` | `/etc/git-signer/git-signer.env` on the signer VM |
| `SIGNER_COMMITTER_NAME` / `SIGNER_COMMITTER_EMAIL` | Passed to `deploy/install-client.sh` on each agent VM |
| `git config user.name` / `user.email` | Written by the installer (user-level) |
| Verified emails on the GitLab account | GitLab → *Edit profile → Emails* |

If the committer email is not a verified email on the account, the commit is stored
and pushed normally but is **Unverified** — no error is raised anywhere.

## Verified-badge checklist

After registering the key and provisioning an agent VM:

1. **Key registered** — the key appears under *SSH Keys* with usage **Signing**
   (not *Authentication & Signing*), and its fingerprint matches
   `ssh-keygen -lf /var/lib/git-signer/signing_key.pub`.
2. **Committer email verified** — `SIGNER_COMMITTER_EMAIL` (and the agent's
   `git config user.email`) is listed and verified under the account's emails.
3. **Commit is signed locally** — on the agent VM:
   ```bash
   git commit --allow-empty -m "verified check"
   git verify-commit HEAD          # exit 0, "Good \"git\" signature for <email>"
   ```
4. **Push to a disposable repository** — push the commit to a throwaway project on
   the same account.
5. **GitLab shows Verified** — open the commit in GitLab; the badge next to the
   committer reads **Verified**. Record the commit URL (or a screenshot) as
   evidence.

The committed version of this checklist, with the exact steps and evidence slots,
is [acceptance-checklist.md](acceptance-checklist.md) (ticket #12) — the manual gate
that must pass before production use.

## Troubleshooting

| Symptom | Likely cause | Where to look |
| --- | --- | --- |
| Commit pushed, badge absent/Unverified | Committer email is not a verified email on the account, or the key is not registered as Signing | [troubleshooting.md](troubleshooting.md) |
| `git commit` fails with exit `128`, nothing pushed | The signer refused the signature (identity, allowlist, rate limit, committer mismatch, …) | [troubleshooting.md](troubleshooting.md) |
| Local `git verify-commit` exits `1` | The key is missing from `gpg.ssh.allowedSignersFile` | [troubleshooting.md](troubleshooting.md) |
| Local `git verify-commit` exits `128` | Corrupt/embedded-garbage signature | [troubleshooting.md](troubleshooting.md) |

Security model behind the registration choice: [security.md](security.md). Key
rotation and revocation (removing the GitLab key): [key-lifecycle.md](key-lifecycle.md).
