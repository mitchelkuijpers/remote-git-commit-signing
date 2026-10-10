# remote-git-commit-signing

Centralized SSH commit signing for exe.dev agent VMs: one persistent signer keeps the only private signing key, and ephemeral agents get their commits signed over an authenticated HTTP seam without ever holding key material.

## Language

**Pinned key**:
The single trusted public key that every client verifies signatures against and that GitLab holds as the Signing-only key. Identified by type and base64 blob only; comments are ignored.
_Avoid_: signing key (ambiguous with the private key), trusted key

**Pinned committer identity**:
The one name/email pair the signer will name in a signature, GitLab checks as the verified committer email, and the client's git config provides via `user.name`/`user.email`. One concept, three consumers; only the committer is checked — the author is deliberately not.
_Avoid_: developer identity, user identity

**Signer base URL**:
The one address of the signer that the server publishes, clients pin, and the installer reaches through. Renders into the client bootstrap; one concept, one name (`SIGNER_URL`), either side of the signing seam.
_Avoid_: public URL, proxy URL

**Allowlist**:
The fail-closed set of source-VM identities permitted to sign; a comma-separated list of exact names or glob patterns, empty means nobody.
_Avoid_: allow list, whitelist

**Source-VM identity**:
The platform-verified caller identity presented in the trust header and matched against the Allowlist; it cannot be set by the client itself.
_Avoid_: VM identity, peer identity

**Trust header**:
The HTTP header carrying the source-VM identity, stamped by the exe.dev peer proxy in production and by devproxy in local setups; no client can forge it.
_Avoid_: identity header
