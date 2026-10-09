# Troubleshooting

Observed failure classes with their symptoms and fixes, plus the exit-code
semantics you need to read test output and CI logs.

## Triage in three commands

```bash
journalctl -u git-signer.service -n 50 --no-pager     # on the signer VM
curl -s http://git-signer.int.exe.xyz/healthz          # from the agent VM (liveness)
curl -s http://git-signer.int.exe.xyz/readyz           # readiness: 503 until the key loads
```

If `readyz` is not `200`, signing cannot work: the key is not loaded. See
[systemd startup failures](#systemd-startup-failures).

## Reading the audit log

Every signing decision — success or rejection — emits exactly one JSON line:

```bash
journalctl -u git-signer.service -f | grep sign_request
# {"time":"...","level":"INFO","msg":"signing decision","event":"sign_request",
#  "outcome":"signed","vm":"agent-1","request_id":"3f...","payload_sha256":"...",
#  "payload_bytes":217,"status":200,"duration_ms":12}
```

| Field | Meaning |
| --- | --- |
| `outcome` | `signed`, `rejected` (4xx), or `failed` (5xx) |
| `vm` | The platform-verified source VM identity (empty when the identity header was missing) |
| `request_id` | Per-request identifier, also returned in the `X-Request-Id` response header — correlate a client report with this log line |
| `payload_sha256` | SHA-256 of the exact payload bytes — correlate with a commit to see who signed it |
| `payload_bytes` | Payload size |
| `status` | HTTP status returned to the client |
| `duration_ms` | Handling time |
| `reason` | Rejection reason code: `malformed_commit`, `pre_existing_signature`, `committer_mismatch`, `payload_too_large` |
| `error` | Signer-side error, when the outcome is `failed` |

Payload bytes and key material are never logged.

```bash
# Who asked for signatures, and what was refused, in the last hour?
journalctl -u git-signer.service --since '1 hour ago' -o cat | grep sign_request \
  | grep -o '"outcome":"[a-z]*","vm":"[^"]*"' | sort | uniq -c
```

## Signing failures from `git commit`

The client prints `git-remote-sign: <reason>` on stderr; Git surfaces it as
`error: git-remote-sign: <reason>` and then aborts with
`fatal: failed to write commit object`, exit code **128**. Nothing is committed or
pushed.

| Symptom (client stderr / HTTP status) | Class | Fix |
| --- | --- | --- |
| `request signature from http://…: … connection refused` / `context deadline exceeded` | Signer unreachable | See below |
| `signer returned 401 Unauthorized: missing VM identity` | Identity missing | See below |
| `signer returned 403 Forbidden: VM identity not allowed` | Not allowlisted | See below |
| `signer returned 429 Too Many Requests: rate limit exceeded` | Rate limited | See below |
| `signer returned 409 Conflict: committer identity is not permitted` | Committer mismatch | See below |
| `signer returned 422 Unprocessable Entity: commit already carries a signature` | Already signed | See below |
| `signer returned 413 Request Entity Too Large: payload too large` | Oversized | See below |
| `signer returned 400 Bad Request: malformed commit payload` | Malformed | See below |
| `signer returned 503 Service Unavailable: signing key not loaded` | Not ready | Restart/reconfigure the server |
| `signer returned 500 Internal Server Error: signing failed` | Backend failure | Check the audit log's `error` field and journald |
| `signer returned <status>: <summary>` (other) | Unexpected | Check the signer logs and the peer proxy |

The client never echoes a raw response body: it prints the HTTP status plus a
sanitized summary (first line, printable ASCII only, length-capped), so a
misconfigured or hostile signer cannot inject terminal escapes or dump bytes into
your terminal. A body with nothing printable is reported by status alone.

### Signer unreachable (git commit exit 128)

```text
error: git-remote-sign: request signature from http://127.0.0.1:1/v1/sign:
  Post "http://127.0.0.1:1/v1/sign": dial tcp 127.0.0.1:1: connect: connection refused
fatal: failed to write commit object
```

Causes and fixes, in order:

1. `GIT_REMOTE_SIGNER_URL` is wrong or points at a localhost/non-peer address. It must
   be the exe.dev peer URL (`http://git-signer.int.exe.xyz`) or a proxy that stamps the
   platform identity. Check with `curl -s "$GIT_REMOTE_SIGNER_URL/healthz"`.
2. The peer integration is not attached to this VM (missing `agent` tag, or the VM was
   attached to neither tag nor name). See [exe-dev-setup.md](exe-dev-setup.md).
3. The signer service is down or restarting:
   `systemctl status git-signer.service`.
4. A network/firewall change on the signer VM — the peer proxy needs the target's web
   port (`8000` by default).

The client does not retry and has no unsigned fallback: it fails the commit.

### Identity missing (401) / foreign (403)

```text
error: git-remote-sign: signer returned 401 Unauthorized: missing VM identity
error: git-remote-sign: signer returned 403 Forbidden: VM identity not allowed
```

- **401** means the request reached the server without the platform identity header.
  Either the URL is not the peer-integration URL (a direct request is refused by
  design), or a proxy between the VM and the signer stripped the header.
- **403** means the VM *does* have an identity but it does not match
  `SIGNER_ALLOWLIST`. The audit line shows the VM name in `vm`:

  ```bash
  journalctl -u git-signer.service -n 5 --no-pager | grep '"status":403'
  ```

  Add the VM name (or a glob such as `agent-*`) to `SIGNER_ALLOWLIST` in
  `/etc/git-signer/git-signer.env` and `sudo systemctl restart git-signer.service`.
  Remember the allowlist is fail-closed: empty admits nobody.

### Rate limited (429)

```text
error: git-remote-sign: signer returned 429 Too Many Requests: rate limit exceeded
```

Each VM has its own token bucket: `SIGNER_RATE_PER_MIN` tokens per minute refilled
continuously, capacity `SIGNER_RATE_BURST` (defaults `60`/`10`). Limiters are
in-process, so a restart resets them.

- Wait a few seconds — the bucket refills continuously.
- For legitimate bulk work, raise `SIGNER_RATE_PER_MIN` / `SIGNER_RATE_BURST` in
  `/etc/git-signer/git-signer.env` and restart.
- Do not retry in a tight loop: the client does not retry, but a wrapper that does
  will just keep hitting the limit. Exhausting one VM's bucket does not affect
  another VM's.

### Committer mismatch (409)

```text
error: git-remote-sign: signer returned 409 Conflict: committer identity is not permitted
```

The payload's `committer` line does not match `SIGNER_COMMITTER_NAME` /
`SIGNER_COMMITTER_EMAIL`. The signer pins the committer because GitLab verifies it.

- Check the VM's effective identity: `git config --global user.name; git config --global user.email`
  (and any repo-local override).
- Re-run `deploy/install-client.sh` with the correct
  `GIT_SIGNER_COMMITTER_NAME`/`GIT_SIGNER_COMMITTER_EMAIL`, or fix the git config
  directly.

### Already carries a signature (422)

```text
error: git-remote-sign: signer returned 422 Unprocessable Entity: commit already carries a signature
```

The payload already has a `gpgsig`/`gpgsig-sha256` header. Git strips `gpgsig` before
handing a payload to the signing program, so this usually means:

- a tool is POSTing an already-signed commit object to `/v1/sign` directly; or
- a wrapper invoked the signing program on a complete commit object rather than the
  signing buffer.

The signer refuses it to avoid acting as a re-signing or arbitrary-signing oracle.
Use a normal `git commit` (or `git commit --amend` on an unsigned commit).

### Oversized (413)

```text
error: git-remote-sign: signer returned 413 Request Entity Too Large: payload too large
```

The payload exceeds **1 MiB**. Commit payloads are normally a few hundred bytes to a
few kilobytes; this indicates the request is not a normal commit payload (or an
absurdly large commit message). Nothing to configure — fix the caller.

### Malformed (400) and other protocol errors

```text
error: git-remote-sign: signer returned 400 Bad Request: malformed commit payload
```

The body is not a structurally valid git commit object (missing/invalid
`tree`/`author`/`committer`, bad header syntax, no blank separator). As with 413, this
normally means something other than Git is calling `/v1/sign`.

For completeness: `405 Method Not Allowed` for a non-POST to `/v1/sign`, `404` for an
unknown path, `503` while the key is not loaded.

## Client configuration errors

These fail before any HTTP request; the same `error: git-remote-sign: …` +
`fatal: failed to write commit object` (exit 128) shape applies.

| Message | Cause | Fix |
| --- | --- | --- |
| `git-remote-sign: GIT_REMOTE_SIGNER_URL is required` | Variable unset (or empty) | Re-run the installer, or `export` it / source `~/.config/git-remote-signer/env` |
| `git-remote-sign: GIT_REMOTE_SIGNER_URL: unsupported scheme "…"` / `missing host` | Bad URL | Use `http://` or `https://` with a host |
| `git-remote-sign: GIT_REMOTE_SIGNER_PUBLIC_KEY is required` | Pinned key unset | Re-run the installer, or set it to the key line or its file path |
| `git-remote-sign: GIT_REMOTE_SIGNER_PUBLIC_KEY: not a valid SSH public key line and not a readable file: …` | Neither a key line nor an existing file | Point it at `~/.config/git-remote-signer/signing.pub` |
| `git-remote-sign: GIT_REMOTE_SIGN_TIMEOUT: …` | Unparsable/non-positive duration | Use a Go duration such as `10s` |
| `git-remote-sign: ssh-keygen not found in PATH: …` | OpenSSH client missing | Install `openssh-client` |
| `git-remote-sign: operation "…" is not implemented yet` | Git called an operation the client does not intercept | Only `sign` is intercepted; check the Git version's argv |
| `git-remote-sign: unsupported signing-program invocation: argv[1] is "…", want -Y` | Wrong invocation | The program must be configured as `gpg.ssh.program`, not called by hand |
| `fatal: either user.signingkey or gpg.ssh.defaultKeyCommand needs to be configured` | Git config incomplete (this error comes from Git, not the client) | Re-run the installer |

## Pinned-key mismatch

Two distinct checks can fail; both are fail-closed.

**1. The key Git asks to sign with is not the pinned key:**

```text
error: git-remote-sign: signing key /path/to/other.pub does not match the pinned GIT_REMOTE_SIGNER_PUBLIC_KEY key
```

`user.signingkey` (passed as `-f`) and `GIT_REMOTE_SIGNER_PUBLIC_KEY` disagree.
Re-run the installer, or set `user.signingkey` to the pinned key file.

**2. The installer's cross-check against the server fails:**

```text
install-client: error: pinned public key does not match the signer's public key
install-client: error: refusing to configure a client with a key the signer does not hold (is a key rotation in progress?).
```

The signer currently holds a different key than the one being pinned. Either the pin
is stale (a rotation happened — update `GIT_REMOTE_SIGNER_PUBLIC_KEY` and re-run) or
the URL points at the wrong signer. Never "fix" this by removing the check.

**3. The server returns a signature that fails local verification:**

```text
error: git-remote-sign: signature failed local verification against pinned key: …
```

The server answered 200 but the signature is not a valid signature over the payload
by the pinned key. Do not bypass: inspect the server (key swapped? a proxy mangling
the body?) and the client's pinned key.

## `git verify-commit` exit codes

Verification never contacts the signer; it runs local `ssh-keygen` through the
client's verbatim passthrough.

| Exit | Meaning | What to check |
| --- | --- | --- |
| `0` | Valid signature by a key in the allowed-signers file | — |
| `1` | Signature valid for some key, but no principal matched — the signer's key is not in `gpg.ssh.allowedSignersFile` | Re-run `deploy/install-client.sh`, which writes `<config>/allowed_signers` mapping the committer email to the pinned key; only hand-edit that file if the key was pinned by hand |
| `128` | Corrupt or unverifiable signature | The commit embeds a bad signature — inspect `git log --show-signature`; this should be impossible through this client, which validates before writing |

- `git log --show-signature` is **exit-code lenient**: it prints
  `No principal matched.` or `No signature` but still exits `0`. Assert on
  `git verify-commit` in tests and CI, not on `--show-signature`.
- Missing configuration reports
  `gpg.ssh.allowedSignersFile needs to be configured and exist for ssh signature verification`
  and shows `No signature`. `deploy/install-client.sh` provisions this file
  (`<config>/allowed_signers`); re-run it rather than configuring the path by hand.
- The passthrough preserves `ssh-keygen`'s exit code exactly; a failure to start
  `ssh-keygen` at all returns `1`.
- Git's verify protocol is two calls (`find-principals`, then `verify`), with
  `check-novalidate` as a fallback; all three are delegated verbatim, so stock OpenSSH
  behavior applies.

### OpenSSH version note: `check-novalidate` ignores `-f`

OpenSSH 9.6 accepts and **ignores** the `-f` flag for `check-novalidate`, verifying the
signature against the key embedded in the signature itself. This is why the client
never uses `check-novalidate` to enforce its pin: it uses `ssh-keygen -Y verify` with a
temporary allowed-signers file containing only the pinned key, which checks the
payload, the `git` namespace, and the key all at once. (The passthrough still forwards
`check-novalidate` because Git uses it as its fallback during `git verify-commit`.)

If you write your own wrapper around the client, do not use `check-novalidate` as a
trust check — a wrong-key signature would pass it.

## Installer self-test failures

`deploy/install-client.sh` fails loudly if the self-test does not pass; the VM is left
configured, but signing does not work yet.

| Message | Cause | Fix |
| --- | --- | --- |
| `install-client: self-test commit failed. The signer is reachable, so check that GIT_REMOTE_SIGNER_URL is the platform URL that injects the verified VM identity for POST /v1/sign, that this VM is in SIGNER_ALLOWLIST, and that the committer identity matches SIGNER_COMMITTER_NAME/EMAIL.` | The signer is reachable but refused the signing request (401/403/409/429) | Fix the specific rejection (see above); run `--skip-selftest` only when you knowingly configure before the integration is attached |
| `install-client: self-test signature did not verify against the pinned key.` | The commit was signed but does not verify locally with the pinned key | The pinned key does not match what the signer used — check for a rotation in progress |
| `install-client: signer is not reachable at <url> (GET /healthz failed). …` | Same causes as [signer unreachable](#signer-unreachable-git-commit-exit-128) | Fix reachability, then re-run |
| `install-client: error: checksum verification failed for <artifact>` | Release artifact/tag mismatch or a tampered download | Re-check the release tag and assets; use `GIT_REMOTE_SIGNER_BIN` meanwhile |
| `install-client: error: checksums.txt has no entry for <artifact>` | Release missing the artifact for this OS/arch | Check the release; or install a local binary |
| `install-client: error: GIT_REMOTE_SIGNER_PUBLIC_KEY …` | Bad pin, per [client configuration errors](#client-configuration-errors) | Fix the value |

`--skip-selftest` configures the VM without the round-trip; use it only when the
integration is attached later.

## systemd startup failures

The server loads the key synchronously and exits non-zero on a bad configuration, so
failures appear as a `git-signer-server: fatal` JSON line and the unit restarting.

```bash
systemctl status git-signer.service
journalctl -u git-signer.service -n 20 --no-pager
```

| Journal `error` value | Cause | Fix |
| --- | --- | --- |
| `SIGNER_KEY_PATH is required` | Env file missing/unreadable, or `EnvironmentFile=` not loaded | Check `/etc/git-signer/git-signer.env` |
| `SIGNER_COMMITTER_NAME is required` / `SIGNER_COMMITTER_EMAIL is required` | Committer identity not configured | Set both in the env file |
| `SIGNER_PORT: invalid port "…"` / `SIGNER_RATE_PER_MIN: invalid value "…"` | Malformed optional setting | Fix or comment out the line |
| `load signing key: … No such file or directory` | Key not generated or wrong path | `sudo deploy/generate-key.sh` |
| `load signing key: … UNPROTECTED PRIVATE KEY FILE! … Permissions 0644 … bad permissions` | Key is group/world-readable | `sudo chmod 0600 /var/lib/git-signer/signing_key` and fix ownership |
| `listen: listen tcp :8000: bind: address already in use` | Another process is on the port | Find it (`ss -ltnp`) or change `SIGNER_PORT` |

Other patterns:

- `StartLimitBurst` stops an endless crash loop; a service that keeps restarting and
  then stops is usually one of the above repeating.
- A port conflict is reported at startup, before the unit becomes active.
- After editing the env file: `sudo systemctl restart git-signer.service`.
- Graceful stops: `systemctl stop` sends `SIGTERM` and the server drains in-flight
  signatures (up to 25s) before exiting; `TimeoutStopSec=30s` leaves room for that.

See [key-lifecycle.md](key-lifecycle.md) for the full operational runbook.

## Useful commands

```bash
# Signer VM
systemctl status git-signer.service
journalctl -u git-signer.service -f -o cat | grep sign_request
journalctl -u git-signer.service -n 50 --no-pager
sudo systemctl restart git-signer.service

# Agent VM
curl -s "$GIT_REMOTE_SIGNER_URL/healthz"; echo; curl -s "$GIT_REMOTE_SIGNER_URL/readyz"; echo
curl -s "$GIT_REMOTE_SIGNER_URL/v1/public-key"
ssh-keygen -lf ~/.config/git-remote-signer/signing.pub
git config --global --get-regexp '^(user|gpg|commit)\.'
git verify-commit HEAD
git log --show-signature -1
```
