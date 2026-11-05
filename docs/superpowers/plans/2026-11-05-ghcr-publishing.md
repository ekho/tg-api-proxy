# GHCR Publishing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Build and publish the proxy image to GHCR after successful CI, with the approved triggers, tags and architectures.

**Architecture:** Add a publishing job to the existing CI, gated by `check`. Official Docker Actions handle metadata/auth/build; BuildKit and Go handle cross-compilation.

**Tech Stack:** GitHub Actions, official Docker Actions, Docker BuildKit, Go cross-compilation, actionlint.

**Spec:** `docs/superpowers/specs/2026-11-05-ghcr-publishing-design.md`

## Global Constraints

- Pushes to `main`, `v*` tags and manual dispatch; no PR publication.
- `latest` only on `refs/heads/main`; full SHA and original version tags.
- `linux/amd64` and `linux/arm64`; built-in `GITHUB_TOKEN`, publishing-job `packages: write`.
- Pinned official actions; no custom tag-generation/parsing scripts or Go application changes.
- No remote push or registry publication during local verification.

## Review Focus

- Failed checks and PRs cannot reach publication.
- A version tag or manual run from another branch cannot overwrite `latest`.
- Both platform manifests contain binaries compiled for their actual architecture.
- Registry write permission remains restricted to the publishing job.
- EN/RU instructions agree and do not claim that publication already ran.

### Task 1: Configure and verify the publication pipeline

**Files:** `.github/workflows/ci.yaml`, `Dockerfile`, `compose.yaml`, `config.example.yaml`, `README.md`, `README.ru.md`.

**Interfaces:** Existing `check` job remains the gate; `publish-image` consumes official metadata outputs and builds the repository Dockerfile. Compose defaults to the approved GHCR latest image; configs.file and the application schema remain unchanged.

- [x] Add tag/manual triggers and per-ref concurrency to existing CI.
- [x] Add the gated, non-PR publishing job with pinned official actions, scoped token permissions and metadata rules from the spec.
- [x] Use BuildKit target arguments in the Dockerfile and native builder platform.
- [x] Set the default Compose image to ghcr.io/ekho/tg-api-proxy:latest, add the requested comment in config.example.yaml, and document image naming, triggers, tags and usage in both READMEs.
- [x] Run actionlint, BuildKit checks and a real two-platform OCI build; inspect architecture metadata and extracted binaries.
- [x] Run the pinned official metadata-action against the manual `latest` tag regression and normal branch/version cases; actionlint validates the workflow expressions.
- [x] Obtain independent read-only review and address actionable findings; run affected checks and commit using Conventional Commits with a co-author footer.

User approved the complete scheme and native execution preference carries forward. No further implementation choice needs approval.
