# Signing key lifecycle

This runbook covers the operational lifecycle of the single ED25519 signing key
held by `git-signer-server` on the persistent signer VM: where it lives, how it
is created, backed up, rotated, and recovered, and how to operate the systemd
service that uses it.

The key is the root of trust for every commit signed by every agent VM. Treat it
like a production credential: it must never appear in an environment variable,
on a command line, in a log line, or in shell history.

## Where the key lives

| Item | Value |
| --- | --- |
| Private key | `/var/lib/git-signer/signing_key` |
| Public key | `/var/lib/git-signer/signing_key.pub` |
| Key directory | `/var/lib/git-signer` (mode `0700`, owner `git-signer:git-signer`) |
| Private key mode | `0600`, owner `git-signer:git-signer` |
| Service account | `git-signer` (system user, no login shell, no home directory) |
| Configuration | `/etc/git-signer/git-signer.env` (mode `0640`, `root:git-signer`; `SIGNER_*` settings, see [Configuration](#configuration)) |
| Unit | `/etc/systemd/system/git-signer.service` |
| Binary | `/usr/local/bin/git-signer-server` |

The server only ever receives the key *path* (`SIGNER_KEY_PATH`). It reads the
file itself, so the key material never reaches the process environment or the
command line. `ssh-keygen` is the only reader and it is invoked with `-f <path>`.

## Initial deployment

On a fresh signer VM, provide the required configuration and run the installer:

```sh
sudo env \
  GIT_SIGNER_COMMITTER_NAME='Your Name' \
  GIT_SIGNER_COMMITTER_EMAIL='you@example.com' \
  GIT_SIGNER_ALLOWLIST='agent-*,builder-1' \
  deploy/install-server.sh
```

The installer creates the `git-signer` account, the `0700` key directory,
`/etc/git-signer/git-signer.env`, installs the binary and the unit, generates a
key if none exists, and enables + starts `git-signer.service`. It prints the
public key at the end; register it with GitLab (below).

It refuses to run without root, and — for a real (non-staged) install — without
the required configuration above, because a server with no allowlist denies
every request.

To install without starting the service (for example while building an image),
use `--no-start`; `DESTDIR=/some/root` stages the files without needing root.

## Configuration

All configuration lives in `/etc/git-signer/git-signer.env`, which the unit
loads via `EnvironmentFile=`. It holds settings only — never key material:

| Setting | Required | Meaning |
| --- | --- | --- |
| `SIGNER_KEY_PATH` | yes | Path to the private signing key |
| `SIGNER_COMMITTER_NAME` | yes | Pinned committer name |
| `SIGNER_COMMITTER_EMAIL` | yes | Pinned committer email (must be verified on the GitLab account) |
| `SIGNER_ALLOWLIST` | yes | Comma-separated VM identities allowed to sign |
| `SIGNER_PORT` | no | Listen port (default `8000`) |
| `SIGNER_RATE_PER_MIN` | no | Sustained per-VM signing rate (default `60`) |
| `SIGNER_RATE_BURST` | no | Per-VM burst capacity (default `10`) |

`SIGNER_ALLOWLIST` entries are matched against the exe.dev-verified source VM
identity (`X-Exedev-Source-Vm`); each entry is either an exact VM name or a
`path.Match` glob (for example `agent-*`). It is **fail-closed**: an empty or
missing allowlist admits nobody, so never leave it unset.

After editing the env file, `sudo systemctl restart git-signer.service`.

## Key generation

```sh
sudo deploy/generate-key.sh
```

The script creates an ED25519 key **without a passphrase**. This is deliberate:
the service runs unattended and has no terminal to unlock a passphrase. The key
is protected by file permissions and by keeping it off every other host; a
passphrase would only add an operational failure mode, not security.

The script never prints private key material — only the public key line is
written to stdout. It refuses to overwrite an existing key unless `--force` is
given (rotation), in which case the previous key is kept as `signing_key.old`.

## Registering the key with GitLab

Register `/var/lib/git-signer/signing_key.pub` on **your personal GitLab
account**, in the user settings under *SSH Keys*:

1. Open <https://gitlab.com/-/user_settings/ssh_keys> (self-managed: your
   instance's equivalent).
2. Paste the *entire* public key line, including the `git-signer ...` comment.
3. Set **Usage** to **Signing only**.

The key **must be Signing-only**. Do **not** enable Authentication and do not
add it as a Deploy key:

- Signing-only grants no shell, no clone, and no push access. A leaked signing
  key can therefore forge commit signatures but cannot touch repositories or the
  GitLab account.
- With Authentication enabled the same key would become a full SSH login
  credential, which is exactly the blast radius we are trying to avoid.

Commits are shown as *Verified* when the signature is valid **and** the commit's
committer email matches an email verified on the registered account. Keep
`SIGNER_COMMITTER_NAME` / `SIGNER_COMMITTER_EMAIL` in
`/etc/git-signer/git-signer.env` set to that identity.

## Pinning the public key on clients

Agent VMs do not fetch the public key from the signer and trust it. They pin it:

- `GIT_REMOTE_SIGNER_PUBLIC_KEY` on each agent VM (or baked into the VM
  image / provisioning) must contain the same line that is registered with
  GitLab.
- `GET /v1/public-key` is informational only. The client compares what it
  fetches against the pinned value and must never blindly accept a new key.

**Whenever the signing key changes, every client's pinned key must change with
it** — this is the first thing to update during rotation (see below). Agent VMs
install the pinned key with `deploy/install-client.sh`, which also cross-checks it
against `GET /v1/public-key` and fails hard on mismatch; re-run it after a
rotation.

## Backup

The private key cannot be regenerated: if it is lost, every previously signed
commit's GitLab *Verified* status still depends on the public key remaining
registered, and you cannot sign again until a new key is registered everywhere.

1. Copy the private key to an offline, encrypted location (password manager
   attachment, encrypted archive on operator-controlled storage):

   ```sh
   sudo sh -c 'umask 077; tar -czf - -C /var/lib/git-signer signing_key signing_key.pub' \
     | gpg --encrypt --recipient you@example.com > git-signer-key-backup.tar.gz.gpg
   ```

   Write the backup with restrictive permissions and delete it from the VM once
   it is safely stored. Prefer a mechanism that never writes plaintext to disk.

2. Record, alongside the backup, the key fingerprint so a restore can be
   verified:

   ```sh
   ssh-keygen -lf /var/lib/git-signer/signing_key.pub
   ```

3. Verify the backup by restoring it into a scratch directory and deriving the
   public key, confirming it matches the fingerprint above:

   ```sh
   mkdir -m 0700 /tmp/keycheck
   gpg --decrypt git-signer-key-backup.tar.gz.gpg | tar -xzf - -C /tmp/keycheck
   ssh-keygen -y -f /tmp/keycheck/signing_key | diff - <(cut -d' ' -f1,2 /tmp/keycheck/signing_key.pub)
   rm -rf /tmp/keycheck
   ```

Never store a plaintext copy of the key in a Git repository, a VM image, a
container layer, or an unencrypted snapshot.

## Rotation

Rotate on schedule or after any suspected exposure. Rotation is only complete
when GitLab *and every client* trust the new key.

1. Generate the new key (the old one is kept as `signing_key.old`):

   ```sh
   sudo deploy/generate-key.sh --force
   ```

   Record the new public key and its fingerprint.

2. Register the new public key with GitLab (Signing-only), exactly as above.
3. Update the pinned `GIT_REMOTE_SIGNER_PUBLIC_KEY` on every agent VM / image,
   and re-provision (the provisioning scripts install the pinned key).
4. Verify a real signing round-trip end to end: on an agent VM run
   `git commit --allow-empty -m "rotation check"` and confirm the commit is
   signed and shows as Verified in GitLab.
5. Only then remove the *old* key from GitLab, and remove
   `signing_key.old` from the VM once you are certain no client still pins it.

During the overlap both keys may be registered; that is expected and is what
makes rotation non-disruptive.

## Recovery from loss

If the private key is lost or corrupted (for example the VM disk is gone and the
backup is unusable):

1. Restore from backup if possible: place the file at
   `/var/lib/git-signer/signing_key`, `chown git-signer:git-signer`, `chmod 0600`,
   then `sudo systemctl restart git-signer`. The GitLab registration and client
   pins remain valid, so nothing else changes.
2. If no backup exists, the key is unrecoverable:
   - Remove the old public key from GitLab (it can no longer sign anything).
   - Generate a new key: `sudo deploy/generate-key.sh`.
   - Register the new public key with GitLab as Signing-only.
   - Update the pinned key on all clients.
   - Commits signed with the lost key remain verifiable only if that key stays
     registered; once removed, their GitLab *Verified* badge is gone. This is
     unavoidable and is the cost of losing the key — which is why backups matter.

## Operating the service

```sh
systemctl status git-signer.service
journalctl -u git-signer.service -f          # structured JSON logs
sudo systemctl restart git-signer.service    # reloads config and key
sudo systemctl stop git-signer.service
```

- Logs are JSON on stderr and land in journald. Payload bytes and key material
  are never logged; signing events carry a payload hash only.
- `Restart=on-failure` restarts the service after crashes but not after an
  operator-requested stop. `StartLimitBurst` stops an endless crash loop so the
  failure stays visible.
- **Graceful stop:** `systemctl stop` sends `SIGTERM`. The server stops accepting
  new connections and drains in-flight signatures (up to 25s) before exiting;
  `TimeoutStopSec=30s` gives it that window before systemd escalates. A stop
  therefore never drops a signature a client is waiting for.

### Fail-fast startup

The server loads the key synchronously at startup, so a broken key stops the
service immediately with a clear journald error instead of failing the first
signature:

```sh
journalctl -u git-signer.service -n 20 --no-pager
```

Typical failures and fixes:

| Symptom in the journal | Cause | Fix |
| --- | --- | --- |
| `SIGNER_KEY_PATH is required` | env file missing or unreadable | check `/etc/git-signer/git-signer.env` |
| `load signing key: ... no such file` | key not generated / wrong path | `sudo deploy/generate-key.sh` |
| `bad permissions` / `UNPROTECTED PRIVATE KEY FILE` | key is group/world readable | `sudo chmod 0600 /var/lib/git-signer/signing_key` (and fix ownership) |

Runtime rejections (the service is up but refuses to sign) are recorded as
`"outcome":"rejected"` audit lines with the requesting VM identity and HTTP status
(`401` when the platform identity header is missing, `403` when it is not
allowlisted). The usual cause is a missing entry in `SIGNER_ALLOWLIST`; add the
VM name (or a glob) and restart the service. Rate limiting shows up the same way
(`429`) once a VM exceeds `SIGNER_RATE_PER_MIN` / `SIGNER_RATE_BURST`.

## Hardening summary

`git-signer.service` runs as the unprivileged `git-signer` user with no
capabilities, `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp`, and a restricted syscall/address-family set. The filesystem is
read-only to the service: the key directory is readable but not writable, and
`ssh-keygen` scratch files live in the per-service private `/tmp`. Only
`/etc/git-signer/git-signer.env` configures it, and that file never contains key
material.

## Related documentation

- [Architecture](architecture.md) — components, trust boundaries, request flows, and
  the complete configuration surface.
- [GitLab setup](gitlab-setup.md) — registering the key (Signing-only), the verified-email
  requirement, and the Verified-badge checklist.
- [Security](security.md) — threat model and the accepted residual risk.
- [Troubleshooting](troubleshooting.md) — failure classes, verify exit codes, systemd
  startup failures.
- [Acceptance checklist](acceptance-checklist.md) — the manual GitLab gate.

