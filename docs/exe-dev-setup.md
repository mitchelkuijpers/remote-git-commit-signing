# exe.dev setup

How to connect agent VMs to the signer using exe.dev's authenticated VM-to-VM
(peer) integration, and what provisioning a new agent VM looks like.

The peer integration is what makes the identity trustworthy: it strips any
`X-Exedev-Source-Vm` header a caller sends and replaces it with one the platform
vouches for, so the signer never has to take the caller's word for who it is.
Reference: <https://exe.dev/docs/integrations-vm-to-vm>,
<https://exe.dev/docs/integrations-attach>.

## Prerequisites

- A persistent VM to host the signer (the "signer VM"), with the server deployed
  (`deploy/install-server.sh`) and its public key registered with GitLab as
  Signing-only. See [key-lifecycle.md](key-lifecycle.md).
- Admin access to your exe.dev account (`exe.dev` CLI, or the exe.dev web UI).

## 1. Deploy the signer on its VM

On the signer VM:

```bash
sudo env \
  GIT_SIGNER_COMMITTER_NAME='Your Name' \
  GIT_SIGNER_COMMITTER_EMAIL='you@example.com' \
  GIT_SIGNER_ALLOWLIST='agent-*' \
  deploy/install-server.sh
```

The server listens on port `8000`, which is the default exe.dev web port, so the
integration target needs no port suffix. `SIGNER_ALLOWLIST` is the *second*
authorization layer on top of the integration: only VM names matching it are
allowed to sign, and it is fail-closed. To make integration attachment the only
gate, set it to `*` (matches every VM) — the tradeoff: any VM ever attached,
including a forgotten tagged one, can sign under the pinned identity. The
reference deployment uses `*` and relies on tag hygiene plus the audit log.

The installer also cross-compiles the client for both Linux architectures and
installs it, with `install-client.sh`, into `SIGNER_DIST_DIR`
(default `/usr/local/lib/git-signer/dist`). The server serves these at
`/v1/client/...` so agent VMs can bootstrap with nothing but the signer URL
(skip with `GIT_SIGNER_SKIP_DIST=1`). Because server and client come from the
same source tree in the same installer run, the served client cannot silently
skew from the server.

## 2. Create the peer integration

Run this from a machine with the `exe.dev` CLI (`ssh exe.dev <command>` from a VM,
or the `exe.dev` command directly). Replace `SIGNER_VM` with the signer VM's name:

```bash
ssh exe.dev integrations add http-proxy \
  --name git-signer \
  --target https://SIGNER_VM.exe.xyz/ \
  --peer \
  --attach tag:agent
```

- `--peer` makes it a VM-to-VM integration: exe.dev generates a key scoped to the
  target VM, stores it server-side, and injects it at the network edge on every
  request. No VM ever holds the key.
- `--attach tag:agent` attaches the integration to a tag, so **any VM carrying the
  `agent` tag** inherits access — including VMs created later. Attach a single VM
  instead with `--attach vm:my-vm`, or to everything with `--attach auto:all`.
- The HTML landing page (`GET /`) is also reachable through the integration, so the
  public key and GitLab instructions are always one `curl` away.

If you prefer the web UI, the [Integrations page](https://exe.dev/integrations) has
an **HTTPS to another VM** tile that does the same thing: pick the target VM
(optionally a port) and attach it to the VMs or tag that should call it.

### Subsequent attachments

Attach or detach the integration later without recreating it:

```bash
ssh exe.dev integrations attach git-signer tag:agent    # tag-based (recommended)
ssh exe.dev integrations attach git-signer vm:my-vm     # a single VM
ssh exe.dev integrations detach git-signer vm:my-vm     # detach
```

Tag an existing VM so it picks up the tag-attached integration:

```bash
ssh exe.dev tag my-vm agent
```

## 3. Reachability

A VM attached to the integration reaches the signer at:

```text
http://git-signer.int.exe.xyz
```

That hostname always resolves through the authenticated peer proxy; the port is the
target VM's web port (`8000` by default). Check it from the agent VM:

```bash
curl -s http://git-signer.int.exe.xyz/healthz          # -> ok
curl -s http://git-signer.int.exe.xyz/v1/public-key    # -> the public signing key
```

Reaching the signer any other way (for example `https://SIGNER_VM.exe.xyz`
directly) does not carry the verified identity, so `POST /v1/sign` refuses it with
`401`. That is intentional: there is no unauthenticated fallback.

## Per-VM story

Provisioning a new agent VM is a tag and one command:

```bash
# once per VM, if it does not already carry the tag
ssh exe.dev tag "$(hostname)" agent

curl -fsSL http://git-signer.int.exe.xyz/install.sh | sh
```

`/install.sh` is rendered by the signer with the pinned configuration baked
in — signer URL, public key, and committer identity. It detects the CPU
architecture, downloads the matching client binary and `install-client.sh`
from the signer's `/v1/client/...` endpoints, and execs the installer. The
bootstrap URL is also linked on the signer's landing page (`GET /`).

To provision without the bootstrap (for example a signer with
`GIT_SIGNER_SKIP_DIST=1`), run `deploy/install-client.sh` from a repo checkout
with the same values exported by hand:

```bash
GIT_REMOTE_SIGNER_URL=http://git-signer.int.exe.xyz \
GIT_REMOTE_SIGNER_PUBLIC_KEY=<public key line or file> \
GIT_SIGNER_COMMITTER_NAME='Your Name' \
GIT_SIGNER_COMMITTER_EMAIL='you@example.com' \
  deploy/install-client.sh
```

The installer checks reachability, cross-checks the pinned key against the server,
installs the binary (from `GIT_REMOTE_SIGNER_BIN` or a checksum-verified release
download), writes the git config and client environment, and runs a signing
self-test that never pushes anything. Re-running it is safe. After that, an
ordinary `git commit` is signed.

In a VM image or provisioning template, run the same bootstrap at first boot;
the pinned public key always comes from the live signer, so key rotation (see
[key-lifecycle.md](key-lifecycle.md)) never strands a baked-in key.

## Zero cleanup on teardown

Destroying an agent VM requires **no** signing-related cleanup:

- There is no private key, token, or credential on the VM — the client only holds
  the public key and its configuration.
- Access is granted by the `agent` tag (or per-VM attachment), not by anything
  stored on the VM, so nothing has to be revoked when the VM disappears.
- The generated peer credential lives server-side on exe.dev and belongs to the
  integration, not the VM.

The only per-VM artifacts are `~/.local/bin/git-remote-sign`,
`~/.config/git-remote-signer/` and the git config written by the installer — all
discarded with the VM's disk.

## Verifying the wiring

From an agent VM, after provisioning:

```bash
git -C /tmp/demo-repo verify-commit HEAD    # if you have a signed commit
git log --show-signature                    # human-readable verification output
```

If signing fails, the commit aborts (exit `128`) rather than producing an unsigned
commit — see [troubleshooting.md](troubleshooting.md).
