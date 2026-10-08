# Spike: Git's `gpg.ssh.program` interface (verified behavior)

> Empirically verified with **Git 2.55.0** and **OpenSSH 9.6p1** using a logging
> fake signer, against a real `$GIT_DIR`. These are the ground-truth facts the
> `git-remote-sign` client must implement. Date: 2026-10-08.

## 1. Sign path

Git invokes the program positionally:

```text
PROG -Y sign -n git -f <user.signingkey> <bufferfile>
```

| Fact | Verified |
|---|---|
| `-f` is exactly the value of `user.signingkey` (a **public key path** in our setup), passed verbatim | ✓ |
| `<bufferfile>` is a temp file (`/tmp/.git_signing_buffer_*`, respects tmpdir), **not** inside the repo | ✓ |
| Signature must be written to `<bufferfile>.sig` before exit | ✓ |
| Buffer contents = commit signing payload: `tree`, `parent`, `author`, `committer`, blank line, message — **headers stripped of the `gpgsig` header, LF-terminated** | ✓ (captured 183/217-byte payloads) |
| Operation is **not** on stdin; nothing is piped for `sign` | ✓ |
| argv is fixed order: `-Y`, `sign`, `-n`, `git`, `-f`, `<key>`, `<file>` | ✓ |

### Failure semantics (important)

| Client behavior | Git result |
|---|---|
| Program exits non-zero | `fatal: failed to write commit object`, **exit 128** — no commit ✓ fail-closed |
| Exit 0, but `<buffer>.sig` missing | `error: failed reading ssh signing data buffer`, **exit 128** — no commit ✓ |
| Exit 0, **garbage** `.sig` written | **Commit SUCCEEDS (exit 0)** and the garbage is permanently embedded in the `gpgsig` header. Only fails later at verify time (`fatal: bad/incompatible signature`, `verify-commit` exit 128) |

**Conclusion:** Git does **zero** validation of `.sig` at commit time. The client
*must* fully validate the returned signature (SSH SIG structure + cryptographic
check against the payload and trusted public key) before writing `.sig` and
exiting 0. A bad signature from the server must never reach the `.sig` file.

## 2. Verify path — TWO invocations, not one

`git log --show-signature` / `git verify-commit` drive a two-step protocol:

**Step 1 — find the principal:**
```text
PROG -Y find-principals -f <allowedSignersFile> -s <sigfile> -Overify-time=<unix-ish ts>
```
- no stdin, no `-n git`
- prints the matched principal on stdout

**Step 2 — verify against that principal:**
```text
PROG -Y verify -n git -f <allowedSignersFile> -I <principal> -s <sigfile> -Overify-time=<ts>  < payload-on-stdin
```
- **payload arrives on stdin** (sigfile is the sig, payload via stdin)
- `-f` here is the **`gpg.ssh.allowedSignersFile`**, **not** a key — delegation must not rewrite it.

**Fallback:** if `find-principals` exits non-zero (no principal matched), Git falls back to:
```text
PROG -Y check-novalidate -n git -s <sigfile> -Overify-time=<ts>  < payload-on-stdin
```
and reports `Good signature ... No principal matched.`

**Conclusion:** the client must implement `verify`, `find-principals`, and
`check-novalidate`, all by passthrough to the real `ssh-keygen` — preserving
stdin, argv, and exit code. `-Overify-time=` equals the commit's own timestamp.

## 3. Verification tooling exit codes (for our tests)

| Command / state | Exit |
|---|---|
| `git commit` with signer failure | 128 |
| `git verify-commit` on valid signature | 0 |
| `git verify-commit`, key not in allowed signers | 1 |
| `git verify-commit` on garbage/corrupt signature | 128 |
| `git log --show-signature` (verification output only) | 0 even on unmatched principal (`No principal matched.`) |
| Missing `gpg.ssh.allowedSignersFile` | error `gpg.ssh.allowedSignersFile needs to be configured and exist...`, shows `No signature` |

**Conclusion:** integration tests must assert on `git verify-commit` exit codes
(strict), not on `git log --show-signature` exit (which is lenient).

## 4. Implementation lessons for `git-remote-sign`

1. **Determine the operation positionally** — `$1 = -Y`, `$2 = op`. Never substring-match:
   a glob like `*sign*` falsely matched `find-principals` **and** a temp path
   (`/tmp/sign-spike/...`) during this spike, corrupting delegation. This is exactly
   the class of bug the parser must not have.
2. **Only `sign` is intercepted.** `verify`, `find-principals`, `check-novalidate` are
   exec-passthrough to real `ssh-keygen` with argv/stdin/exit-code preserved.
3. **Validate before write before exit:** read payload → POST → receive signature →
   cryptographically verify signature against payload + pinned pubkey → **then**
   atomically write `<buffer>.sig` → exit 0. Any failure → delete partial `.sig`, exit non-zero.
4. **`user.signingkey` is the *public* key path on agent VMs.** The client checks
   that `-f` matches its pinned trusted public key, but never needs the private key.
5. Namespace must be `git` (`-n git`); anything else for `sign` → fail loudly.
6. Signature temp dir is world-accessible (`/tmp`) — write the `.sig` atomically
   (`O_EXCL`/temp+rename) and clean up on failure.

## Evidence

Raw argv/stdin logs from the spike runs are reproducible with the harness under
`/tmp/sign-spike/` (ephemeral): wrapper scripts `fake-signer`, `real-signer`,
`capture-signer` + real `$GIT_DIR` at `/tmp/sign-spike/repo`. See command
transcript in session history (2026-10-08).
