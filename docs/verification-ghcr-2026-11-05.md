# GHCR workflow verification — 2026-11-05

Implemented GitHub Actions image publishing and the user-approved default image
`ghcr.io/ekho/tg-api-proxy:latest` in Compose. `config.example.yaml` mentions the
image in a comment; its application schema is unchanged. Both READMEs describe
the same pull/run behavior and tag rules.

## Local verification

- `actionlint` v1.7.12: PASS for the final workflow.
- BuildKit `--check` for `linux/amd64,linux/arm64`: PASS, no warnings.
- Real multi-platform OCI image build: PASS. Platform manifests and image configs
  contain `amd64` and `arm64`; extracted application binaries were verified with
  `file` as x86-64 and ARM AArch64 respectively. Both retain UID/GID 65534.
- Rendered Compose configuration: GHCR `latest`, no local build entry, and the
  existing `/config/config.yaml` config target preserved.
- Final `go test -race ./... -count=1`: PASS, all three packages.
- Git whitespace validation: PASS with the existing Go-compatible settings.

## Independent review and regression check

A fresh read-only reviewer found one Important issue: metadata-action's
`type=ref,event=tag` rule also processes a Git tag selected for a manual run.
Dispatching on a tag literally named `latest` could therefore add `:latest`
without passing the explicit main-branch condition.

The issue was reproduced with the actual pinned metadata-action
`dc802804100637a589fabce1cb79ff13a1411302`, its bundled Node entrypoint, and a local
HTTP fixture for the GitHub API. The assertion failed because `:latest` appeared
for `workflow_dispatch` on `refs/tags/latest`.

The version-tag rule now uses metadata-action's existing `enable` option and
GitHub expressions, restricting it to pushes of `refs/tags/v*`. No custom tag
implementation or library patch was added. After resolving the workflow's
boolean expressions for each event/ref fixture, the same official action passed:

| Event/ref | Generated tags |
|---|---|
| Manual run on Git tag `latest` | Full SHA only |
| Push to `main` | `latest` and full SHA |
| Push Git tag `v1.2.3` | `v1.2.3` and full SHA |
| Manual run on a feature branch | Full SHA only |
| Manual run on Git tag `v1.2.3` | Full SHA only |
| Manual run on `main` | `latest` and full SHA |

The reviewer found no Critical or Minor issues. The Important issue was fixed in
one pass and verified by the failing-then-passing case, all six metadata cases,
actionlint, and the full race suite. No findings remain deferred.

## Boundaries

The checks did not push Git refs, authenticate to GHCR, publish a registry image,
or run the hosted GitHub workflow. Actual package permissions, package visibility
and successful remote publication remain unverified until the workflow is pushed
and run on GitHub. Local metadata checks used fixture API responses and test-only
authentication data, not real GitHub credentials.
