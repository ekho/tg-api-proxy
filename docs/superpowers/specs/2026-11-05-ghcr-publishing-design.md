# GHCR image publishing

Approved by the user on 2026-11-05: extend the existing CI to publish
`ghcr.io/ekho/tg-api-proxy` after successful checks on pushes to `main`, pushes
of `v*` tags, and manual dispatch. Publish `linux/amd64` and `linux/arm64`.

## Approach

Use an image job in the existing workflow with `needs: check`. This directly
couples publication to the checks for the same revision. A separate workflow
would require additional cross-workflow coordination, so it is not selected.
Pull requests run checks and cannot publish images.

Use official Docker login, Buildx, metadata and build/push actions, pinned to
verified commit SHAs. Authenticate with the built-in `GITHUB_TOKEN`; grant
`packages: write` only to the publishing job and keep `contents: read` otherwise.
Generate tags with metadata-action, without custom shell/tag parsing:

- `latest` only for `refs/heads/main`;
- `sha-<full commit SHA>` for every publication;
- the original `v*` Git tag for tag pushes; tags must not automatically add `latest`.

Use native Go cross-compilation with BuildKit's `BUILDPLATFORM`, `TARGETOS` and
`TARGETARCH`. No QEMU, custom scripts or application changes are needed. The user subsequently
approved making `ghcr.io/ekho/tg-api-proxy:latest` the default Compose image,
removing its local `build` entry and mentioning the image in a comment in
`config.example.yaml`. Keep the application schema and `configs.file` unchanged.
Document pulling/running the image and optional local builds in both READMEs.

## Verification and boundaries

Validate the workflow with actionlint and Dockerfile with BuildKit checks.
Build a real OCI image for both target architectures and inspect the image
manifests and binaries. Review CI gating, permissions and tag conditions.
No remote Git push or live GHCR publication is part of this local implementation.
Actual registry permissions and a hosted workflow run remain unverified until
it is pushed and run in GitHub.
