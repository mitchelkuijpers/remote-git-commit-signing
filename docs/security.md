# Security

Threat model, trust boundaries, and what the system does and does not guarantee.

## Assets

| Asset | Why it matters |
| --- | --- |
| The ED25519 private signing key | Root of trust for every commit signed by every agent VM; compromise allows forging commits attributed to the developer |
| The GitLab account's verified identity | The account emails that make the **Verified** badge meaningful |
| Repository integrity | What gets merged and released; signing is evidence, not authorization |
| The audit trail | Needed to reconstruct who asked for which signature |

## Actors and assumptions

| Actor | Assumed capability |
| --- | --- |
| Agent VM (authorized) | Can run arbitrary code in the VM, including calling the signer as itself |
| Agent VM (unauthorized / arbitrary internet client) | Can attempt any HTTP request; cannot obtain a platform-vouched identity |
| exe.dev platform | Honest and correct: it authenticates the source VM and sets the identity header |
| Operator | Trusted; owns the signer VM, the env file, the key backups, and the GitLab account |
| GitLab | Honest; independently validates signatures and verified emails |

Out of scope: compromise of the exe.dev platform itself, compromise of the operator's
workstation or GitLab account, and attacks on Git's own signing/verification code.

## Trust boundaries

1. **Platform → signer (identity).** The server authorizes based on
   `X-Exedev-Source-Vm`, which **must** have been set by exe.dev's authenticated peer
   proxy. exe.dev strips any caller-supplied value of that header and sets its own
   after authenticating the caller ([VM-to-VM integration](https://exe.dev/docs/integrations-vm-to-vm)).
   The signer therefore trusts the header *only* because every request that can reach
   `POST /v1/sign` arrives through that proxy, and it treats requests without an
   identity as `401` — there is no unauthenticated fallback. A request that reaches
   the server by any other route (directly to the VM, a reverse proxy that does not
   authenticate, a stale DNS/IP path) carries no identity and is refused.
2. **Server → client (signature integrity).** The client pins the trusted public key
   in `GIT_REMOTE_SIGNER_PUBLIC_KEY` and verifies every returned signature
   cryptographically against that pinned key before writing `.sig`. A broken,
   misconfigured, or hostile server cannot make an agent commit under a key the client
   does not trust.
3. **Signer → host (key material).** The private key is read from disk by the server
   process (and its `ssh-keygen` child) and never appears in the environment, on a
   command line, or in a log line. The service runs unprivileged with
   `ProtectSystem=strict`, `PrivateTmp`, no capabilities, and a restricted syscall
   set.
4. **Repository (out of scope).** The signing payload contains no trustworthy
   repository identity, so the system authorizes at the **VM** level only. It cannot
   distinguish a request intended for a throwaway repo from one intended for a
   protected branch. See the residual risk below.

## What the platform guarantees

- The `X-Exedev-Source-Vm` value that reaches the app was vouched for by the platform:
  the peer integration carries a short-lived signed attestation bound to the target VM,
  and the header is stripped on every other path, including a person setting it on a
  direct request.
- The generated peer credential lives server-side at exe.dev's edge, scoped to the
  target VM, and is never readable by any VM.
- It names the **source VM, not a user**. Anyone who can run code on an authorized VM
  can call the signer *as that VM*. This is the fact the residual risk turns on.

**Do not trust `X-Exedev-Source-Vm` (or any copy of it) without this integration.**
If the signer is exposed on a route that does not authenticate the caller — a raw
public port, a tunnel, a proxy that forwards the header verbatim — the app's identity
check becomes forgeable header trust. The supported deployments are (a) the exe.dev
peer URL, and (b) loopback for local development. `scripts/devproxy`, used by the local
demo, is precisely such a header-forwarding stand-in: it is **development-only** and
must never be deployed in front of a production signer.

## Accepted residual risk

**An authorized VM can request a signature for an arbitrary commit under the
developer's identity.** The signer proves *an authorized VM asked*, not *the developer
approved*. A compromised or misbehaving authorized VM can therefore produce commits
that GitLab marks Verified, covering any content (subject to the commits-only
validation below). This risk is **accepted** and must be mitigated operationally:

| Mitigation | How |
| --- | --- |
| Audit log | Every signing decision emits one structured `slog` JSON line with the VM identity, payload SHA-256, HTTP status and duration — so every signature can be traced to the VM that requested it, and correlated with a commit by its payload hash |
| Protected branches and review | Require merge requests and human review on protected branches; the **Verified** badge proves authorship keys, not that the change is wanted. Repository policy is the real gate on what lands |
| Revocation | Removing the key from GitLab (or rotating it, see [key-lifecycle.md](key-lifecycle.md)) immediately stops all commits from verifying |
| Allowlist | `SIGNER_ALLOWLIST` limits which VMs can sign at all; it is fail-closed (empty admits nobody) |
| Rate limiting | Per-VM token bucket caps how much a runaway or compromised VM can sign |
| Commit-only validation | The signer refuses anything that is not a structural commit object, so it cannot be used as a general-purpose SSH signing oracle |

Because the author identity is deliberately not checked (GitLab verifies the
committer only), a VM can also set an arbitrary *author* while the committer stays
pinned. Do not treat the author field as trustworthy either.

A deployment that wants tag/VM attachment to be the only gate can set
`SIGNER_ALLOWLIST=*`: the mechanism stays in place but matches every VM. The
tradeoff is that any VM ever granted the integration — including an old tagged
VM you forgot about — can sign commits under the pinned identity. Audit logging
still traces every signature to the calling VM.

## Client bootstrap endpoints

`GET /`, `GET /v1/public-key`, `GET /install.sh`, and `GET /v1/client/...` are
unauthenticated, like the landing page: they expose no key material, only the
public key and already-public configuration. On exe.dev the service's web port
is reachable only through your own account (edge-authenticated or the peer
integration), so the audience is account-scoped.

- `GET /install.sh` renders the pinned committer identity (name and email) into
  the bootstrap script — the same identity stamped on every public commit.
- `GET /v1/client/...` serves the client binaries and installer over the same
  channel the design already trusts for signing: a network position that could
  tamper with those downloads could already substitute commit payloads
  mid-sign. The installer additionally cross-checks the pinned key against the
  signer's `/v1/public-key` before installing anything.
- The file endpoint serves a fixed whitelist of names from `SIGNER_DIST_DIR`;
  no other files in that directory are exposed, and the URL name is never
  spliced into a filesystem path.

## What the pinned client key protects against

`GIT_REMOTE_SIGNER_PUBLIC_KEY` is the client's independent source of truth:

- A **key-substitution attack**: a server that returns signatures from a different key
  (or from an attacker's key) is rejected, because the client's local
  `ssh-keygen -Y verify` checks against the pinned key, not the signature's embedded
  key. (This is why `check-novalidate` is deliberately not used: OpenSSH 9.6 accepts
  and ignores its `-f` flag, which would verify against the embedded key.)
- **Silent corruption**: Git embeds a `.sig` without validating it at commit time; the
  client validates *before* writing, so garbage can never become a commit.
- **Key rotation drift**: the installer cross-checks the pinned key against
  `GET /v1/public-key` and refuses to configure a client with a key the signer does not
  hold. Note the server derives its served public key once at startup (a deliberate
  fail-fast choice), so a rotation is only complete after `systemctl restart
  git-signer.service`; until then the served key is stale and the cross-check fails by
  design. See [key-lifecycle.md](key-lifecycle.md#rotation).

## No unauthenticated fallback

- The server rejects identity-less signing requests (`401`) and has no "allow all"
  mode. An unset or empty allowlist refuses every request (`403` semantics for
  authorization, with the empty list matching nothing).
- The client has **no unsigned fallback**: any failure — missing config, unreachable
  signer, non-2xx response, wrong pinned key, failed local verification — removes a
  partial `.sig` and exits non-zero, aborting the commit (Git exit `128`).
- Unknown signing-program operations fail loudly rather than being approximated.

## Defense in depth in the signer

- **Pinned committer:** only commits naming exactly `SIGNER_COMMITTER_NAME` /
  `SIGNER_COMMITTER_EMAIL` are signed.
- **Structural validation:** payloads are parsed as git commit objects (headers with
  continuation lines, blank separator, message body), not regex-matched; a payload that
  already carries `gpgsig`/`gpgsig-sha256` is refused (`422`).
- **Size limits:** payloads over `1 MiB` are refused (`413`); signature responses are
  bounded client-side.
- **No shell:** `ssh-keygen` is invoked with `exec.Command` directly (never a shell),
  with timeouts and unique temp directories that are cleaned up on every path.
- **Logging hygiene:** payload bytes and key material are never logged; the payload
  SHA-256 and a stable reason code are the only payload-derived fields.
- **Least privilege:** dedicated unprivileged account, hardened systemd unit, no
  capabilities, read-only filesystem view, private `/tmp`.
- **Dependency surface:** Go standard library only — no third-party modules.

## Explicit non-claims

- Not a repository authorization system (VM-level only).
- Not an approval or policy engine (see the residual risk).
- Not a secrets manager — it holds one signing key and nothing else.
- Signing does not imply pushing: agents' GitLab push credentials are managed outside
  this project.
- The **Verified** badge is not a statement that a change is correct, reviewed, or
  wanted.

Related: [architecture.md](architecture.md) for the request flows,
[key-lifecycle.md](key-lifecycle.md) for rotation and revocation,
[troubleshooting.md](troubleshooting.md) for failures.
