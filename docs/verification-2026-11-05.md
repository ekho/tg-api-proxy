# Delivery verification — 2026-11-05

Implementation is in local branch `codex/standalone-ip-proxy`. The source checkout was read-only and remains unchanged.

## Automated checks

- Baseline imported-runtime tests: PASS, all three packages.
- Final `go test -race ./... -count=1`: PASS, all three packages.
- `go build ./...`, `go vet ./...`, `gofmt`, and `docker compose config --quiet`: PASS.
- Scan of delivered files for the removed organization name: no matches.
- Whitespace check: PASS with standard Go-compatible Git whitespace settings (the host globally flags tabs in indentation).
- Independent review of `b687548..e265145`: approve; no Critical, Important or Minor findings. Reviewer separately ran parser/reload tests with the race detector: PASS.

## Docker acceptance

Built the actual Dockerfile and ran an isolated Compose project using `configs.file` and a localhost-only ephemeral port. The service ran as UID/GID 65534 with read-only configuration and filesystem. Checks passed:

- `/healthz` returned 200.
- An allowed request reached the deliberately unreachable local outgoing proxy and returned 502. This proves authorization without making a live Telegram call.
- A valid in-place YAML update changed that request to 403.
- An invalid update retained 403 and kept `/healthz` at 200. The unchanged invalid candidate was not repeatedly logged.
- A separate process given the invalid initial YAML exited with code 1.
- Correcting the file restored the allowed result, 502, without changing container ID or restarting the process.
- Host-side file replacement through rename was not visible inside this Docker backend. Force-recreating the container exposed the new file and its 403 policy. The README describes this limitation explicitly.
- Temporary Compose containers and their dedicated network were removed after the check.

## Decisions and verification boundaries

- Work stayed in the requested initially empty repository, on a dedicated branch. The first Git approval attempt failed with HTTP 403; the approval service recovered, and the branch/commits were then created normally. No approval bypass or unresolved Git restriction remains.
- Startup/environment wiring was implemented alongside access integration because removing the old client type changed both interfaces. Startup/env tests were written first, and the full suite and real container cover the integrated result.
- `accessClient` is private, rather than adding a public `Client` wrapper. All application callers compile and are covered by tests.
- Access logs use the same selected IP as authorization. Standalone observation tests use the TCP peer when no trusted middleware has resolved another address.
- Native tests use atomic fixture writes to test deduplication against one stable invalid candidate. Separate same-size in-place and container tests exercise mounted-file updates, including transient read/parse failure fallback.
- Git whitespace verification permits standard Go indentation tabs and checks trailing whitespace; gofmt verifies Go formatting.
- The independent reviewer did not judge the user's external reverse proxy configuration, live Telegram availability, or other Docker backends. These remain outside local implementation acceptance: no external reverse proxy or Telegram endpoint was deployed or exercised, and other backends may differ in rename visibility.
- The result remains a local branch because the approved scope is repository implementation and local checks. No push, pull request or external deployment was requested.

No deferred review findings remain. Actual client IDs/IPs, proxy trust addresses and optional collector/proxy endpoints must be supplied in the user's runtime configuration.
