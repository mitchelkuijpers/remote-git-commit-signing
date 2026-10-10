# Implementation Plan: Remote Git Commit Signing for exe.dev Agents

> **Status:** Implemented (v0.1.0). This document is the original agreed plan, kept for
> the record; what is actually built is described in the [architecture](architecture.md)
> and the rest of the current documentation set linked from the [README](../README.md).
> The one remaining gate is the [manual GitLab acceptance
> checklist](acceptance-checklist.md).

**Goal:** Build a lightweight, secure, centralized Git commit-signing service that allows
dozens of short-lived exe.dev VMs to create SSH-signed commits attributed to your personal
GitLab account.

The solution should require:

- **One SSH signing key** registered with GitLab.
- **Zero private signing keys** on agent VMs.
- **Zero manually managed credentials** for communication between agents and the signing service.
- Automatic signing with ordinary `git commit` commands.
- Minimal configuration when creating new VMs.
- A small, maintainable Go codebase.

We'll use **Go, OpenSSH, Git's custom signing-program interface, and exe.dev's VM-to-VM integrations**.

Two important implementation details verified up front:

1. exe.dev's peer integrations provide authenticated VM-to-VM HTTP communication and a
   platform-verified `X-Exedev-Source-Vm` header. Tag-based attachments can automatically
   grant new VMs access. ([exe.dev VM-to-VM integrations](https://exe.dev/docs/integrations-vm-to-vm))
2. Git's `gpg.ssh.program` interface behaves like `ssh-keygen`, not a generic stdin/stdout
   signing command. For signing, Git passes an input filename and expects the signature in
   `<inputfile>.sig`. The implementation must preserve that behavior.
   ([Git `gpg-interface.c`](https://github.com/git/git/blob/master/gpg-interface.c))

---

## Coding agent prompt

### Project: `git-remote-signer`

You are a senior Go engineer. Build a lightweight remote SSH commit-signing service designed
for ephemeral coding-agent VMs hosted on [exe.dev](https://exe.dev).

The system must integrate transparently with Git and use exe.dev's native VM-to-VM
authentication mechanism.

**Prioritize simplicity, security, and compatibility with standard Git tooling.**

Implement a working, tested prototype. Do not stop after generating scaffolding or writing a
design document.

### 1. Architecture

The system consists of two Go applications:

**A. `git-signer-server`**

A persistent HTTP service running on a dedicated exe.dev VM.

Responsibilities:

- Hold one persistent ED25519 SSH private signing key.
- Accept signing requests from authenticated exe.dev VMs.
- Authorize requests using their verified VM identity.
- Validate commit payloads before signing.
- Produce standard OpenSSH signatures in the `git` namespace.
- Return signatures to clients.
- Log signing activity without exposing sensitive content.
- Never expose the private key.

**B. `git-remote-sign`**

A lightweight CLI installed on ephemeral agent VMs.

Responsibilities:

- Implement Git's `gpg.ssh.program` interface.
- Intercept SSH signing requests.
- Send commit data to `git-signer-server`.
- Write the returned signature where Git expects it.
- Delegate local signature-verification operations to the real `ssh-keygen`.
- Fail cleanly when signing cannot be completed.
- Require no private signing key, GitLab API token, or other signing-service credential.

The architecture should look like this:

```text
                     GitLab
                       │
               Registered public
                 signing key
                       │
              ┌────────┴────────┐
              │                 │
              │  git-signer     │
              │  Persistent VM  │
              │                 │
              │  Private key    │
              │  HTTP API       │
              │  Audit logs     │
              │                 │
              └────────▲────────┘
                       │
                  exe.dev peer
                  integration
                       │
       ┌───────────────┼───────────────┐
       │               │               │
  ┌────┴─────┐    ┌────┴─────┐    ┌────┴─────┐
  │ Agent A  │    │ Agent B  │    │ Agent C  │
  │          │    │          │    │          │
  │ Git      │    │ Git      │    │ Git      │
  │ Client   │    │ Client   │    │ Client   │
  └──────────┘    └──────────┘    └──────────┘
```

There must be no need to register a new signing key when an agent VM is created.

### 2. Technology choices

Use the following technologies:

| Component | Technology |
|---|---|
| Language | Go |
| Signing algorithm | ED25519 |
| Signature format | OpenSSH SSHSIG |
| Git integration | `gpg.ssh.program` |
| HTTP server | Go standard library |
| Authentication | exe.dev peer integration |
| Authorization | Verified VM identity |
| Configuration | Environment variables or config file |
| Server deployment | systemd |
| Client distribution | Standalone Go binary |
| Logging | Go `log/slog` |

Avoid unnecessary dependencies.

The server and client should be independently deployable.

Target Linux AMD64 and ARM64 for the initial release. Structure the client so macOS support
can be added without redesigning the protocol.

### 3. Git integration

This is the most important technical requirement.

Git supports SSH commit signing through:

```bash
git config --global gpg.format ssh
git config --global gpg.ssh.program git-remote-sign
git config --global commit.gpgsign true
```

It also needs a signing key:

```bash
git config --global user.signingkey \
  "$HOME/.config/git-remote-signer/signing.pub"
```

The public key is identical across all agent VMs.

#### Implement the OpenSSH signing interface correctly

Git's signing interface is not a simple stdin/stdout protocol.

Git invokes the configured program approximately as follows:

```bash
git-remote-sign -Y sign \
  -n git \
  -f /path/to/signing.pub \
  /path/to/git-signing-buffer
```

The program must:

1. Parse the arguments.
2. Require the `git` signing namespace.
3. Validate the requested signing key against the configured public key.
4. Read the input file as raw bytes.
5. Submit those exact bytes to the signing API.
6. Receive a standard OpenSSH SSHSIG signature.
7. Write the signature to `<input-file>.sig`.
8. Return exit code 0 only after successfully writing the complete signature.

The signature must be compatible with standard `ssh-keygen` verification.

Implement atomic file creation where practical. Remove partial signature files on failure.

**Important:** Git also uses its configured SSH program for signature verification.

For commands such as:

```bash
ssh-keygen -Y verify
ssh-keygen -Y find-principals
ssh-keygen -Y check-novalidate
```

delegate execution to the system's actual `ssh-keygen`.

Only intercept supported signing requests. Never silently sign arbitrary data when invoked
with unexpected arguments.

Use the [Git source implementation](https://github.com/git/git/blob/master/gpg-interface.c)
as the reference for command-line compatibility.

### 4. Signing API

Implement a minimal versioned HTTP API.

#### `POST /v1/sign`

Accept raw commit data:

```http
POST /v1/sign HTTP/1.1
Content-Type: application/octet-stream
X-Exedev-Source-Vm: agent-example

<raw git commit payload>
```

The identity header must be supplied and authenticated by exe.dev's peer integration. It
must never be accepted merely because an arbitrary client provided that header.

The request body contains the exact bytes Git needs signed.

Return the SSHSIG signature as the raw response body:

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.sshsig

-----BEGIN SSH SIGNATURE-----
...
-----END SSH SIGNATURE-----
```

Use appropriate HTTP status codes for authentication failures, authorization failures,
invalid payloads, oversized requests, and internal errors.

#### `GET /v1/public-key`

Return the configured SSH public signing key.

This allows clients to retrieve and validate the public key during provisioning.

The client must use a pinned or preinstalled trusted public key. It must not blindly trust a
newly retrieved key.

#### `GET /healthz`

Return basic service health.

Do not expose key material, internal configuration, or other sensitive information.

#### `GET /readyz`

Report whether the signing service is ready to handle requests.

Readiness should verify that a signing key is available without exposing it.

### 5. Signing implementation

For the initial implementation, use OpenSSH's existing signing functionality instead of
implementing SSHSIG cryptography manually.

For example:

```bash
ssh-keygen -Y sign \
  -f /var/lib/git-signer/signing_key \
  -n git \
  /tmp/input
```

This generates the signature file.

The server should:

- Use a dedicated, access-restricted working directory.
- Create uniquely named temporary files for concurrent requests.
- Enforce strict input-size limits.
- Invoke `ssh-keygen` without a shell.
- Apply timeouts to signing operations.
- Clean up temporary files.
- Return only the generated signature.
- Never log private keys, complete request bodies, or sensitive commit messages.

Using the `os/exec` package is acceptable for the prototype.

Keep the actual signing implementation behind a Go interface so another signing backend
could be introduced later.

### 6. exe.dev authentication

Use exe.dev's documented VM-to-VM peer integrations.

Documentation: <https://exe.dev/docs/integrations-vm-to-vm>

For example, the integration should be configurable using:

```bash
ssh exe.dev integrations add http-proxy \
  --name git-signer \
  --target https://SIGNER_VM.exe.xyz/ \
  --peer \
  --attach tag:agent
```

Replace `SIGNER_VM` with the actual VM name.

Agent VMs with the `agent` tag can then reach the service through:

```text
https://git-signer.int.exe.xyz
```

The integration authenticates requests and provides the verified caller VM identity.

#### Authorization requirements

The signing service must:

1. Reject requests without an authenticated VM identity.
2. Use the platform-verified `X-Exedev-Source-Vm` identity.
3. Support an allowlist of permitted VM names or patterns.
4. Log which VM requested each signature.
5. Apply configurable rate limits.
6. Never accept a VM identity supplied through an unauthenticated public endpoint.

**Do not treat an arbitrary HTTP header as proof of identity.**

In production, trust the identity header only when the request has passed through exe.dev's
authenticated peer proxy.

Verify that the deployed service cannot be accessed through another route that bypasses that
authentication.

Do not implement a fallback that accepts unauthenticated requests.

Tag-based integration attachment controls which VMs can reach the signer. Server-side
authorization should provide an additional restriction.

### 7. Commit identity and validation

The GitLab account belongs to a human developer, and the agents should create commits
attributed to that developer.

The signer should enforce a configured committer identity:

```text
SIGNER_COMMITTER_NAME="Developer Name"
SIGNER_COMMITTER_EMAIL="developer@example.com"
```

Before signing, parse the Git commit payload and validate that:

- The payload is a structurally valid Git commit signing payload.
- The committer name matches the configured identity.
- The committer email matches the configured email.
- The payload is within configured size limits.
- The request uses the `git` signing namespace.
- The payload does not contain a preexisting embedded signature that would make processing ambiguous.

Do not rely solely on a regular expression for commit parsing. Account for multiline Git
headers and normal Git commit structure.

For the MVP, support regular Git commit signatures. Signed tags can be a later enhancement.

A request with an unexpected identity must be rejected.

Importantly, **GitLab verifies the committer's identity**, not necessarily the author's
identity. GitLab requires the committer email to match a verified email on the GitLab account
holding the signing key.

#### Repository authorization limitation

The signing payload does not provide a trustworthy GitLab repository identity.

Therefore, the service must not claim to enforce repository-level authorization based solely
on a repository URL or path supplied by the client.

For this prototype, use VM-level authorization and document that limitation.

Repository-specific authorization can be considered in a future version with an independently
verifiable authorization mechanism.

### 8. Client configuration

Make the client configuration extremely simple.

The following environment variables should be sufficient:

```bash
GIT_REMOTE_SIGNER_URL=https://git-signer.int.exe.xyz
GIT_REMOTE_SIGNER_PUBLIC_KEY=$HOME/.config/git-remote-signer/signing.pub
```

The client must support a configurable timeout, with a reasonable default.

It must:

- Use HTTP timeouts.
- Reject oversized responses.
- Validate the returned signature.
- Ensure the signature corresponds to the submitted data and trusted public key.
- Never silently fall back to unsigned commits.
- Return useful error messages.
- Avoid excessive retries.
- Avoid writing commit contents to logs.

The experience should be transparent:

```bash
git add .
git commit -m "feat: implement something"
```

There should be no additional commands required during normal operation.

### 9. Provisioning

Provide an installation script that can run on a fresh exe.dev agent VM.

For example:

```bash
./install-client.sh
```

It should:

1. Install the appropriate `git-remote-sign` binary.
2. Install or validate the trusted public key.
3. Configure Git to use SSH signing.
4. Enable automatic commit signing.
5. Configure the developer's Git name and verified email.
6. Verify that the signing service is reachable.
7. Execute a harmless signing self-test without pushing anything to GitLab.

The script should be idempotent.

It should work without Nix, Docker, or a heavyweight runtime.

It should never download and execute unverified binaries. Support checksum verification or an
equivalent trusted distribution mechanism.

The resulting configuration should work with an existing dotfile management solution.

### 10. Server deployment

Provide a systemd service for deployment on a persistent exe.dev VM.

The signer should run under a dedicated unprivileged Linux account.

Suggested key path:

```text
/var/lib/git-signer/signing_key
```

Requirements:

- Restrictive file permissions.
- No root execution during normal operation.
- Automatic service restart on unexpected failures.
- Structured logs visible through `journalctl`.
- Minimal filesystem access.
- Appropriate systemd hardening.
- Graceful shutdown.
- No private key in environment variables, command-line arguments, or logs.

Provide scripts or documentation for generating the signing key securely.

The public key should be registered with the developer's GitLab account as **Signing** only.

Document key backup, rotation, and recovery.

### 11. Testing

Create comprehensive automated tests.

#### Unit tests

Cover:

- Argument parsing.
- Public key validation.
- Commit payload parsing.
- Committer identity validation.
- Authorization policies.
- HTTP request validation.
- HTTP error handling.
- Signing timeouts.
- Concurrent requests.
- Response size limits.
- Signature file creation and cleanup.

#### Integration tests

Build an end-to-end test using a real temporary Git repository and a test signing key.

The test should:

1. Start the signing server.
2. Configure the client to use the server.
3. Initialize a Git repository.
4. Create a file.
5. Execute a normal `git commit`.
6. Verify that Git created a signed commit.
7. Verify the signature using standard OpenSSH tooling.
8. Verify the correct signing key was used.

The integration tests must not require a real GitLab account.

Also test:

- Invalid signature responses.
- Incorrect committer email.
- Unauthorized callers.
- Unavailable signing service.
- A signing request that times out.
- Concurrent commits.
- Local verification through `git log --show-signature`.

Include tests proving that failures do not silently produce unsigned commits.

Mocked identity-header tests are useful for unit testing, but they are not sufficient to prove
authentication security.

Document a live exe.dev test that verifies direct unauthenticated requests cannot spoof the VM
identity.

#### Real-world GitLab validation

Document an optional manual acceptance test:

1. Register the public signing key with GitLab.
2. Configure an agent VM.
3. Commit using the developer's verified GitLab email.
4. Push to a disposable test repository.
5. Verify GitLab displays the commit as **Verified**.

This is an acceptance requirement before production use.

### 12. Observability

Implement structured logging using `slog`.

Each signing request should log:

```json
{
  "event": "commit_signed",
  "vm": "agent-123",
  "request_id": "3f9c…",
  "status": "success",
  "duration_ms": 12
}
```

Useful additional fields include a request ID and a cryptographic hash of the signing payload.

Never log the complete commit payload.

Implement basic counters for successful, rejected, and failed signing requests.

Do not introduce a metrics infrastructure dependency for the MVP.

### 13. Security expectations

Treat agent VMs as potentially compromised.

Assume an attacker may control an agent process and submit arbitrary signing requests from an
authorized VM.

The service must protect the private key, but it cannot guarantee that authorized agents only
request signatures for safe code.

Specifically:

- Signing permission must not imply GitLab push permission.
- Signing must not bypass merge-request reviews.
- The service must not expose administrative operations to agent VMs.
- The signing key must never leave the server.
- Unexpected input must fail closed.
- Disable any signing fallback that would undermine verification.
- A compromised authorized VM may request unauthorized commits to be signed; document this
  residual risk.

Avoid adding complex security features that are not needed for the initial implementation, but
do not compromise these requirements.

### 14. Project structure

Use a clean Go project layout.

Suggested structure:

```text
git-remote-signer/
├── cmd/
│   ├── git-signer-server/
│   │   └── main.go
│   └── git-remote-sign/
│       └── main.go
├── internal/
│   ├── api/
│   ├── auth/
│   ├── client/
│   ├── config/
│   ├── git/
│   └── signing/
├── deploy/
│   ├── git-signer.service
│   ├── install-server.sh
│   └── install-client.sh
├── docs/
│   ├── architecture.md
│   ├── exe-dev-setup.md
│   ├── gitlab-setup.md
│   ├── security.md
│   └── troubleshooting.md
├── go.mod
├── Makefile
└── README.md
```

Adjust the layout where appropriate, but keep the codebase simple.

### 15. Development milestones

Implement the project incrementally.

**Milestone 1 — Local proof of concept**

Build the signer server and client, communicating over localhost.

Generate a temporary signing key and demonstrate a real Git commit signed through the remote
service.

Verify the result cryptographically.

This milestone must work before proceeding to platform integration.

**Milestone 2 — exe.dev integration**

Configure the server to accept requests through exe.dev's authenticated peer integration.

Implement authorization based on verified VM identity.

Document the setup commands.

**Milestone 3 — Agent provisioning**

Implement the client installation script.

Make configuration portable between short-lived VMs.

Ensure repeated installation does not cause problems.

**Milestone 4 — Security and reliability**

Add comprehensive validation, timeouts, rate limiting, safe logging, systemd hardening, and
failure handling.

Verify the trust boundary of exe.dev authentication.

**Milestone 5 — Documentation and release**

Complete the README and deployment documentation.

Build standalone Linux binaries.

Provide an end-to-end example and a checklist for validating GitLab's Verified status.

### 16. Definition of done

The project is complete when:

- A persistent exe.dev VM can host the signing service.
- A new ephemeral exe.dev VM can be provisioned using a single installation command.
- The ephemeral VM requires no private signing key.
- A normal `git commit` creates a valid SSH-signed commit.
- The signed commit is verifiable with standard SSH tooling.
- GitLab recognizes the signing key and marks commits with the correct committer email as Verified.
- Multiple VMs can sign commits concurrently using the same central key.
- The service rejects unauthorized signing requests.
- Destroying an ephemeral VM requires no signing-key cleanup.
- Signing failures do not create unsigned commits.
- All automated tests pass.
- The README contains instructions for deploying the complete system.

### 17. Implementation guidelines

Start by examining the exact interface between Git and `ssh-keygen`.

Build a minimal end-to-end prototype before adding advanced functionality.

Prefer boring, well-understood Go code.

Do not introduce databases, message queues, containers, Kubernetes, or other infrastructure
unless demonstrably necessary.

Do not build a custom cryptographic protocol.

Keep private key operations on the server.

Avoid unnecessary abstractions, but design the signing backend and authentication checks so
they can be replaced independently.

Make reasonable implementation decisions without repeatedly requesting clarification.

When finished, provide:

1. A summary of the implemented architecture.
2. Instructions to run the server locally.
3. Instructions to install the client.
4. exe.dev deployment commands.
5. GitLab configuration instructions.
6. Test results.
7. Any remaining security limitations or incomplete functionality.

**The primary success criterion is that an ordinary `git commit` from an ephemeral exe.dev VM
produces a valid SSH-signed commit using a private key that exists only on the persistent
signer VM.**

---

## A few design decisions made up front

There are three choices worth emphasizing beyond the implementation prompt.

### 1. Don't expose the signing service directly to the internet

Use the authenticated exe.dev peer integration as the **only permitted path for signing
requests**.

The signing endpoint should be unreachable from ordinary unauthenticated internet clients. This
is especially important because all signatures will be associated with your personal GitLab
identity.

exe.dev documents that its peer integration authenticates the caller and provides a verified
source-VM identity. The implementation should rely on that mechanism rather than inventing
another token-management system.
([exe.dev](https://exe.dev/docs/integrations-vm-to-vm))

### 2. Treat signing and pushing as separate problems

This service solves commit signing. Agents will still need independent GitLab authentication
to push their commits.

Keep those permissions separate:

| Permission | Mechanism |
|---|---|
| Sign commits | Central signing service |
| Clone repositories | Existing GitLab authentication |
| Push branches | Appropriately scoped GitLab credentials |
| Merge to protected branches | GitLab permissions and approval rules |

That prevents the signing service from becoming an unnecessarily powerful GitLab integration.

### 3. Keep the first version small

Explicitly avoid a database, Kubernetes, external secret manager, or web dashboard for the
prototype.

The initial deployment can be:

**One Go server + one ED25519 key + one systemd service + one small Go client.**

The result should be easy to deploy, inspect, and maintain.

Later, if needed, add a hardware-backed signing key, per-VM policies, improved audit trails, and
support for locally sandboxed agents.

## Useful reference documentation

- [exe.dev — VM-to-VM Integration](https://exe.dev/docs/integrations-vm-to-vm)
- [exe.dev — Attaching Integrations](https://exe.dev/docs/integrations-attach)
- [Git — Signing Implementation](https://github.com/git/git/blob/master/gpg-interface.c)
- [GitLab — SSH Signed Commits](https://docs.gitlab.com/user/project/repository/signed_commits/ssh/)
- [Git — Configuration Reference](https://git-scm.com/docs/git-config)

One important caveat: **the centralized signer can establish that an authorized VM requested a
signature, but not that you personally approved the commit.** Since GitLab will display these
commits under your identity, preserve VM-level audit information and continue requiring
reviews for protected branches.

With that boundary understood, this architecture should scale comfortably to dozens of
disposable VMs without creating a signing-key management problem.
