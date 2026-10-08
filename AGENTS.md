# remote-git-commit-signing

Remote Git commit signing for exe.dev agents. See [README.md](README.md).

## Agent skills

### Issue tracker

Issues live as GitHub issues on this repo; use the `gh` CLI (on exe.dev VMs, via the
`GH_HOST`/`GH_TOKEN` proxy env vars. See `docs/agents/issue-tracker.md`.

### Triage labels

Canonical five-role vocabulary with default label strings (`needs-triage`, `needs-info`,
`ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: root `GLOSSARY.md` + `docs/adr/`, read before exploring. See
`docs/agents/domain.md`.
