# Architecture

How the remote commit-signing system is put together: the components, the trust
boundaries between them, the two request flows (sign and verify), where the key
material lives, and the complete configuration surface.

For the original agreed design see [implementation-plan.md](implementation-plan.md);
for the consolidated specification see [spec.md](spec.md). This document describes
what is implemented.

## Components

| Component | Where it runs | What it is |
| --- | --- | --- |
| `git-signer-server` | One persistent signer VM | HTTP service holding the single ED25519 private signing key; authenticates, authorizes, rate-limits and validates signing requests, then signs via `ssh-keygen -Y sign` |
| `git-remote-sign` | Every agent VM | Git `gpg.ssh.program`: forwards the exact commit payload to the signer, verifies the returned signature locally, writes `<buffer>.sig`; delegates verification operations to the real `ssh-keygen` |
| exe.dev peer integration | Platform | Authenticated VM-to-VM HTTP path; attaches VMs (by tag or name) to the signer and delivers the platform-verified source-VM identity |
| `deploy/install-server.sh` + `git-signer.service` | Signer VM | Hardened systemd install of the server on a dedicated unprivileged account |
| `deploy/install-client.sh` | Agent VM | One-command idempotent client provisioning |
| `scripts/demo-local.sh`, `scripts/devproxy` | Dev machine | Local end-to-end demo; `devproxy` stands in for the exe.dev peer proxy by stamping the identity header |

Go standard library only, one module, two binaries built from `cmd/`. Shared code
lives under `internal/`: `server` (HTTP, config, authz, rate limit, landing page,
commit validation), `signing` (the `Signer` interface and the `ssh-keygen`
backend), `gitobj` (structural git-object parsing), `client` (argv parsing,
config, sign, verify passthrough).

## Trust boundaries

```text
        (untrusted)                 (platform boundary)              (trusted)
   ┌─────────────────┐          ┌──────────────────────┐        ┌──────────────────┐
   │  agent VM /     │  HTTP    │  exe.dev authenticated│  HTTP │  signer VM:      │
   │  any caller     │ ───────▶ │  peer proxy           │ ─────▶ │  git-signer-     │
   │                 │          │  • strips client-     │        │  server + private│
   │  git-remote-sign│          │    supplied identity  │        │  key             │
   │  (pins pubkey)  │          │  • sets X-Exedev-     │        └──────────────────┘
   └─────────────────┘          │    Source-Vm          │
                                └──────────────────────┘
```

1. **Platform identity boundary.** The `X-Exedev-Source-Vm` header is set by
   exe.dev's authenticated peer proxy *after stripping anything the caller sent*.
   The server trusts this header only because every request that can reach
   `POST /v1/sign` arrives through that proxy. The value names a VM, not a user:
   anyone who can run code on a VM can call the signer as that VM (see the residual
   risk below). Details: [exe-dev-setup.md](exe-dev-setup.md) and
   [security.md](security.md).
2. **Client pin boundary.** The client never trusts a key it fetches. The public
   signing key is *pinned* in `SIGNER_PUBLIC_KEY`, and every signature is
   verified locally against that pinned key before it is written. A compromised or
   misconfigured server cannot substitute a key.
3. **Signer boundary.** The private key exists only on the signer VM, readable only
   by the unprivileged `git-signer` service account. It is never placed in the
   environment, on a command line, or in a log line; the server only ever receives
   and passes the key *path* to `ssh-keygen`.
4. **Repository boundary.** Repository-level authorization is **out of scope**: the
   signing payload carries no trustworthy repository identity, so authorization is
   VM-level. This is the documented residual risk.

Within the signer, the request pipeline is a strict chain — identity, then
allowlist, then rate limit, then structural commit validation, then signing — so
nothing reaches the signing backend before every earlier check has passed.

## Sign path

1. The developer (or agent) runs `git commit`. Git invokes the configured
   `gpg.ssh.program` positionally:
   `git-remote-sign -Y sign -n git -f <user.signingkey> <bufferfile>`.
2. `git-remote-sign` reads the buffer bytes (the commit signing payload: `tree`,
   `parent`, `author`, `committer`, blank line, message — with any `gpgsig` header
   stripped) and checks that the `-f` key equals its pinned key.
3. It `POST`s the exact bytes to `SIGNER_URL/v1/sign` as
   `application/octet-stream`.
4. The request traverses the exe.dev peer proxy, which sets the platform-verified
   `X-Exedev-Source-Vm` header.
5. `git-signer-server` runs the authorization chain: missing identity → `401`,
   not allowlisted → `403`, rate limit exhausted → `429`. Then it validates the
   payload as a commit object naming the pinned committer: malformed → `400`,
   oversized → `413`, pre-existing `gpgsig` → `422`, wrong committer → `409`.
6. The signing backend runs `ssh-keygen -Y sign -n git` with the private key path,
   in a unique temp directory, with a per-request timeout and input size cap.
7. The server returns the raw SSHSIG (`application/vnd.sshsig`) and emits exactly
   one structured `slog` audit line for the decision: VM, payload SHA-256, status,
   duration — never the payload.
8. The client verifies the signature — well-formed, bound to the `git` namespace,
   and made by the *pinned* key — using `ssh-keygen -Y verify` against a temporary
   allowed-signers file containing only the pinned key. This is why
   `check-novalidate` is not used for this check: OpenSSH 9.6 accepts and ignores
   its `-f` flag, which would verify against the key embedded in the signature
   itself and defeat the pin.
9. Only then does the client write `<bufferfile>.sig` atomically (temp file +
   rename) and exit `0`. Any failure removes a partial `.sig` and exits non-zero,
   so Git aborts the commit (exit `128`) rather than embedding an unverified
   signature.

## Verify path

Verification never contacts the signer. Git drives its own two-step protocol
through the configured program, and `git-remote-sign` delegates it verbatim to the
system `ssh-keygen`, preserving argv, stdin/stdout and the exit code:

1. step 1 — `PROG -Y find-principals -f <allowedSignersFile> -s <sigfile> -Overify-time=<ts>`
   (no stdin); prints the matched principal;
2. step 2 — `PROG -Y verify -n git -f <allowedSignersFile> -I <principal> -s <sigfile> -Overify-time=<ts> < payload-on-stdin`;
3. fallback — if `find-principals` matched nothing, Git calls
   `PROG -Y check-novalidate -n git -s <sigfile> -Overify-time=<ts> < payload`.

In verify mode `-f` is the **allowed-signers file**, not a key, and the client must
not rewrite it. Locally, `git verify-commit` exit codes are the strict signal:
`0` valid, `1` unknown signer (key not in the allowed-signers file), `128`
corrupt signature. `git log --show-signature` is exit-code lenient and reports the
same verification outcome textually.

GitLab verifies independently: it validates the embedded SSHSIG against the public
key registered on the account, and shows the commit as **Verified** when the
signature is valid *and* the commit's committer email matches a verified email on
that account. See [gitlab-setup.md](gitlab-setup.md).

## Key locations

| Item | Where | Notes |
| --- | --- | --- |
| Private signing key | `/var/lib/git-signer/signing_key` | Mode `0600`, `git-signer:git-signer`; only the signer VM |
| Public signing key | `/var/lib/git-signer/signing_key.pub` | Registered with GitLab (Signing-only); served at `/` and `/v1/public-key` |
| Server configuration | `/etc/git-signer/git-signer.env` | Mode `0640`, `root:git-signer`; settings only, never key material |
| Server binary / unit | `/usr/local/bin/git-signer-server`, `/etc/systemd/system/git-signer.service` | |
| Client binary | `~/.local/bin/git-remote-sign` | Overridable with `GIT_REMOTE_SIGNER_INSTALL_DIR` |
| Client pinned key | `~/.config/git-remote-signer/signing.pub` | Public material only |
| Client environment | `~/.config/git-remote-signer/env` | Sourced from `~/.profile` |

Backup, rotation and recovery: [key-lifecycle.md](key-lifecycle.md).

## Configuration surface

### `git-signer-server`

| Environment variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `SIGNER_KEY_PATH` | yes | — | Path to the private signing key (loaded at startup; a broken key fails startup) |
| `SIGNER_COMMITTER_NAME` | yes | — | Pinned committer name; only commits naming it are signed |
| `SIGNER_COMMITTER_EMAIL` | yes | — | Pinned committer email; must be a verified email on the GitLab account |
| `SIGNER_ALLOWLIST` | yes in practice | empty (denies everyone) | Comma-separated VM identities permitted to sign; exact names or `path.Match` globs |
| `SIGNER_PORT` | no | `8000` | TCP listen port |
| `SIGNER_RATE_PER_MIN` | no | `60` | Sustained per-VM signing rate (tokens/minute) |
| `SIGNER_RATE_BURST` | no | `10` | Per-VM token bucket capacity (largest instantaneous burst) |
| `SIGNER_URL` | no | `https://git-signer.int.exe.xyz` | Signer base URL rendered into `/install.sh` and the landing page; must be the exe.dev peer URL (absolute `http`/`https` with a host; empty uses the https default) |

Internal limits not exposed as environment variables: maximum signing payload
`1 MiB`, per-request signing timeout `5s`, HTTP server timeouts (read-header
`5s`, read `10s`, write `30s`, idle `60s`), and a `25s` graceful-shutdown drain
(`TimeoutStopSec=30s` in the unit).

### `git-remote-sign`

| Environment variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `SIGNER_URL` | yes | — | Signer base URL (`http`/`https`); in production the exe.dev peer URL |
| `SIGNER_PUBLIC_KEY` | yes | — | Pinned trusted public key: a literal authorized_keys line, or a path to a file containing one |
| `SIGNER_TIMEOUT` | no | `10s` | Timeout for one HTTP round trip to the signer |

Internal limits: signature response body capped at `64 KiB`; local verification
invocation capped at `10s`.

### Deployment scripts

Per ADR-0001 the legacy installer name-mapping layer is gone: installer inputs
use the same `SIGNER_*` names verbatim.

`deploy/install-server.sh` (run as root; `--no-start`, `--skip-key`; stage with
`DESTDIR`) consumes the server settings directly — `SIGNER_COMMITTER_NAME`,
`SIGNER_COMMITTER_EMAIL` and `SIGNER_ALLOWLIST` are required for a real install,
and `SIGNER_PORT`, `SIGNER_RATE_PER_MIN`, `SIGNER_RATE_BURST` are passed through
to the generated env file. Installer-only knobs:

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `SIGNER_USER` / `SIGNER_GROUP` | no | `git-signer` | Service account |
| `SIGNER_KEY_DIR` / `SIGNER_KEY_NAME` | no | `/var/lib/git-signer` / `signing_key` | Key location |
| `SIGNER_CONF_DIR` | no | `/etc/git-signer` | Env-file directory |
| `SIGNER_SERVER_BIN` | no | — | Prebuilt server binary to install (otherwise built from the repo, or found next to the script / in the repo root) |
| `SIGNER_REPO_DIR` | no | repo root | Source tree used to build the binary |

`deploy/install-client.sh` (`--skip-selftest`):

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `SIGNER_URL` | yes | — | Signer base URL persisted to the client env file |
| `SIGNER_PUBLIC_KEY` | yes | — | Pinned key (line or file), installed and cross-checked against `/v1/public-key` |
| `SIGNER_COMMITTER_NAME` | yes | — | Written to `git config --global user.name` |
| `SIGNER_COMMITTER_EMAIL` | yes | — | Written to `git config --global user.email` |
| `GIT_REMOTE_SIGNER_BIN` | no | — | Local binary to install (skips the download) |
| `GIT_REMOTE_SIGNER_RELEASE_VERSION` | no | `v0.1.0` | Release tag to download |
| `GIT_REMOTE_SIGNER_DOWNLOAD_BASE` | no | this repo's GitHub releases | Download base URL |
| `GIT_REMOTE_SIGNER_INSTALL_DIR` | no | `$HOME/.local/bin` | Binary directory |
| `GIT_REMOTE_SIGNER_CONFIG_DIR` | no | `$XDG_CONFIG_HOME/git-remote-signer` | Config directory |
| `GIT_REMOTE_SIGNER_PROFILE` | no | `$HOME/.profile` | Login profile that sources the env file |
| `SIGNER_TIMEOUT` | no | unset (client default `10s`) | Persisted to the client env file when set |

### HTTP API

| Endpoint | Method | Auth | Success | Errors |
| --- | --- | --- | --- | --- |
| `/` | GET | none | `200` HTML landing page (public key + fingerprint + GitLab steps) | `503` when no key is loaded |
| `/v1/public-key` | GET | none | `200` public key | `503` when no key is loaded |
| `/v1/sign` | POST | `X-Exedev-Source-Vm` + allowlist + rate limit | `200` raw SSHSIG | `400` malformed, `401` no identity, `403` not allowlisted, `405` wrong method, `409` committer mismatch, `413` oversized, `422` already signed, `429` rate limited, `500` signing failure, `503` not ready |
| `/healthz` | GET | none | `200 ok` | — |
| `/readyz` | GET | none | `200 ok` | `503` until the key is loaded |
| anything else | — | — | — | `404` |

Nothing but the public key and its fingerprint is ever exposed by the landing page:
not the key path, the environment, or any other configuration.

## Operational properties

- **Fail-fast startup.** The key is loaded synchronously at startup, so a missing,
  unreadable, or loosely-permissioned key stops the service immediately with a
  clear journald message instead of failing the first signature.
- **Graceful shutdown.** On `SIGTERM`/`SIGINT` the server stops accepting new
  connections and drains in-flight signatures (up to 25s) before exiting.
- **Statelessness.** No database and no on-disk state beyond the key: rate-limit
  buckets and counters (`signed`/`rejected`/`failed`) are in-process and reset on
  restart. Restarting the service is always safe.
- **No unsigned fallback.** Every failure path in the client deletes a partial
  `.sig` and exits non-zero; Git then aborts the commit.
