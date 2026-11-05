# Standalone Telegram Bot API Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Copy the existing proxy into this repository, replace header keys with per-client IP access, and support safe automatic YAML reload under Docker Compose.

**Architecture:** Keep the existing reverse proxy and observability. An immutable access-policy snapshot combines clients and trusted proxies; a background poller validates file updates before atomically publishing them. Each request resolves and authorizes its IP against one snapshot.

**Tech Stack:** Existing Go/OpenTelemetry stack; standard `net/netip`, `time.Ticker`, `os.ReadFile`, `sync/atomic`; candidate modules `go4.org/netipx`, `github.com/goccy/go-yaml`, and `github.com/go-chi/chi/v5/middleware`; Docker Compose.

**Spec:** `docs/superpowers/specs/2026-11-05-standalone-proxy-design.md`

## Global Constraints

- The source checkout is read-only; its Git history and platform operations are not imported.
- Rename the module to `github.com/ekho/tg-api-proxy`; no old organization names, domains, contacts or credentials in delivered files.
- No header-key authentication or mandatory platform tunnel in the result.
- Poll file contents every **1 second**; invalid updates retain the complete last valid policy.
- Configuration contains `trusted_proxies` and `clients`, with client `name`, `bot_ids` and `allowed_ips`.
- Compose must use `configs` with source `proxy_config`, target `/config/config.yaml` and `file: ./config.yaml`; do not substitute a directory bind mount.
- Reload guarantees apply to content visible inside the container. Document that `configs.file` may retain the old file after host-side `rename`; in-place updates or container recreation are needed in that case.
- Shared bot IDs remain supported; reject a request that matches multiple clients.
- Preserve streaming, long polling, token redaction, structured logs and optional telemetry.
- Use standard/community library APIs directly. Do not implement custom IP/range parsers, IP sets, forwarded-header algorithms, configuration frameworks or unnecessary wrappers. Discuss gaps and obtain explicit approval for application-specific validation/reload integration before writing product code.
- No live Telegram calls, external deployment, publication or changes to the source checkout.

## Review Focus

- Same-size edits and replacements visible at the process path must reload; Compose's single-file mount limitation must be documented rather than hidden: Task 2 native reload tests and Task 4 in-place container smoke test.
- Malformed, missing or repeated forwarding headers must not bypass IP restrictions: Task 3 resolver and HTTP integration tests.
- IPv4-mapped peers, IPv6 ranges, cross-family/reversed ranges and inclusive endpoints must behave consistently: Task 2 parser tests.
- Concurrent reload and in-flight requests must use complete snapshots without data races: Task 2 reload and Task 3 race-detector tests.
- Reloaded proxy trust and client permissions must change together, while token-containing paths remain absent from logs/traces: Task 3 integration and retained leak tests.

---

### Task 1: Import the independent runtime and establish a baseline

**Files:** Create `main.go`, `main_test.go`, `go.mod`, `go.sum`, `Dockerfile`, `.gitignore`, and `internal/{tgproxy,telemetry}/` from the source runtime. Do not import its other directories or workflows. Later tasks replace access-specific files and documentation.

**Interfaces:** Preserve `tgproxy.NewHandler(Config) (http.Handler, error)`, `tgproxy.LoadSettings(func(string) string) (*Settings, error)` and existing telemetry interfaces until their owning tasks update the dependencies.

- [ ] Copy only the runtime files and tests; rewrite module imports and instrumentation scope to the new module. Remove obsolete platform-specific commentary as touched.
- [ ] Run `go test ./... -count=1`, `go build ./...`, and `go vet ./...` to establish a source-copy baseline. Record any pre-existing failures before changing behavior.
- [ ] Record the baseline in a local Conventional Commit with a co-author footer; do not push.

### Task 2: Strict YAML policy parsing and last-valid-file reload

**Files:** Replace `internal/tgproxy/clients.go` and `clients_test.go`; create `internal/tgproxy/access_config.go`, `access_config_test.go`, `reload.go`, and `reload_test.go`; update `go.mod`/`go.sum` for the verified modules.

**Interfaces:**
- `ParseAccessConfig(raw []byte) (*AccessConfig, error)` parses the complete YAML policy.
- `(*AccessConfig).Authorize(botID string, ip netip.Addr) *Client` returns a client only for exactly one match.
- Use `atomic.Pointer[AccessConfig]` directly to publish/read complete immutable snapshots; do not add an `AccessStore` wrapper.
- After explicit approval for the described application integration, `WatchAccessFile(ctx context.Context, path string, interval time.Duration, current *atomic.Pointer[AccessConfig], logger *slog.Logger)` polls until cancellation; the caller supplies exactly `time.Second` in production and joins the watcher.

- [ ] Write parser tests before implementation. Assert individual IPv4/IPv6, CIDR and mixed range lists accept matching addresses; inclusive range endpoints match and adjacent addresses do not. Assert empty clients/IPs/bots, duplicate names, nonnumeric IDs, unknown YAML keys, extra YAML documents, malformed/scoped IPs, mixed-family and reversed ranges fail. Assert shared bots work on disjoint IPs and overlapping matching clients return nil.
- [ ] Run the focused tests and confirm the new APIs are missing or assertions fail.
- [ ] Use strict `goccy/go-yaml` decoding, `netip.ParseAddr`/`ParsePrefix`, and `netipx.ParseIPRange`/`IPSetBuilder`/`IPSet.Contains` directly. Do not implement range arithmetic, normalization algorithms or membership structures. Retain numeric bot IDs as strings and do not store bot tokens or proxy keys. Check config constraints without changing library semantics.
- [ ] Write reload tests before implementation: valid A -> invalid B retains A and logs one rejection for unchanged B -> valid C replaces A; deletion retains C and restoration is applied. Include same-size changes and atomic rename. Assert cancellation stops the watcher and concurrent readers see only complete versions.
- [ ] Run the reload tests and confirm failure, then implement content-based polling, deduplicated failures and atomic publication. Reject unreadable/invalid initial files in startup wiring in Task 4.
- [ ] Run `go test ./internal/tgproxy -run 'Test(ParseAccess|Access|Reload|Watch)' -count=1`; commit after the complete suite is green following Task 3's dependent integration.

### Task 3: Trusted client-IP resolution and proxy integration

**Files:** Update `internal/tgproxy/proxy.go`, `observe.go`, `proxy_test.go`, policy construction, and all existing tests using key fixtures, client lookup, authorization status or forwarded address expectations. Add focused middleware-integration tests, without a bespoke `clientip.go` resolver.

**Interfaces:**
- Use Chi's `middleware.ClientIPFromXFF(trustedIPPrefixes ...string)` and `middleware.GetClientIPAddr(ctx)` directly; use `ClientIPFromRemoteAddr` when no proxies are trusted. Validate/compile prefixes before constructing the middleware, since the library's constructor panics on invalid prefixes. Use library APIs to derive prefixes from IP sets.
- Replace `Config.Clients` with `Config.Access *atomic.Pointer[AccessConfig]`; each request obtains one `AccessConfig` and uses the middleware and client rules compiled from that same candidate.
- Logging, tracing and authorization read the same resolved address provided by the middleware context. Do not introduce a second resolver or change the existing router/server.

- [ ] Write application integration tests before wiring the library. Untrusted peers cannot spoof access with any forwarding header. Trusted peers require an identifiable client; repeated header lines follow the module's chain traversal. Right-to-left trust traversal ignores attacker-controlled left prefixes after the first untrusted hop; malformed traversed hops leave the client unknown and access denied. Exercise IPv6 and IPv4-mapped peers. Do not add stricter parsing of unexamined chain entries or duplicate the library's internal test suite.
- [ ] Run the focused integration tests and confirm failure, then wire the existing Chi middleware using the compiled policy rules.
- [ ] Replace proxy tests with source-IP/client tests: allowed bot and IP forward without a header; a supplied key cannot authorize a blocked IP; wrong bot/IP and ambiguous clients return `403`; malformed paths return `400`; `/healthz` remains `200`.
- [ ] Run the changed tests and confirm the old header implementation fails. Wire the policy snapshot into the request path and remove key authentication, hashing, parsing, fixtures and key-management assumptions.
- [ ] Update every dependent logging/metrics/tracing/streaming fixture without dropping its existing assertions. Add an HTTP integration test that changes both trusted proxies and client access in one reload and proves the new policy is used. Preserve leak checks and bound denied-request telemetry labels.
- [ ] Run `go test ./... -count=1` and `go test -race ./... -count=1`; inspect any changed observation expectations. Commit the integrated access/reload change with a co-author footer.

### Task 4: Startup, Compose, documentation and delivery checks

**Files:** Update `main.go`, `main_test.go`, `internal/tgproxy/settings.go`, `settings_test.go`, `internal/telemetry/telemetry.go` comments, and `Dockerfile`; create `compose.yaml`, `config.example.yaml`, `.env.example`, `.dockerignore`, `README.md` and `.github/workflows/ci.yaml`.

**Interfaces:** Keep `LoadSettings(getenv)` for process-level settings and config path. Startup loads/validates the initial policy before listening, starts a watcher with `time.Second`, and cancels/joins it during shutdown. Standard proxy environment variables configure outgoing transport through `http.ProxyFromEnvironment`.

- [ ] Add failing tests for config-path defaults/override, invalid initial configuration, direct upstream operation, optional standard outgoing proxy settings, and shutdown-budget compatibility with Compose. Remove tests of obsolete environment variables.
- [ ] Run these tests, then wire startup and shutdown, optional outgoing proxy transport and immutable process settings. Ensure no configuration read occurs per request.
- [ ] Add a Compose service with `configs: [{source: proxy_config, target: /config/config.yaml}]` and top-level `configs.proxy_config.file: ./config.yaml`, `CONFIG_FILE=/config/config.yaml`, localhost-only `8080`, non-root execution and `stop_grace_period: 75s`. Do not use a directory bind mount. Disable optional telemetry by default in the Compose example. Ignore the real root `config.yaml`, `.env` and local build artifacts; exclude runtime config from the image context. Default local `CONFIG_FILE` to `config.yaml`.
- [ ] Write README instructions: copy `config.example.yaml` to `config.yaml`, set clients and exact trusted proxy addresses, `docker compose up -d --build`, live reload behavior and logs, Nginx forwarding/TLS example, and shared Docker network alternative without published ports. Explain the single-file bind-mount limitation of `configs.file`: changes to the existing file are visible, while rename-based saves may require `docker compose up -d --force-recreate`; the service cannot detect host contents hidden by the mount. Distinguish this from atomic application of a validated policy. Use only synthetic IDs and reserved documentation IPs. Explain that an admitted in-flight request completes after revocation.
- [ ] Add independent read-only CI running build, vet, uncached tests and formatting checks. Do not copy deployment, notification or access-management workflows.
- [ ] Run `gofmt`, `go build ./...`, `go vet ./...`, `go test -race ./... -count=1`, `docker compose config --quiet`, a scan of all delivered files for forbidden names/domains and obsolete auth/tunnel references, and `git diff --check`. Re-run only checks justified by new edits.
- [ ] If the Docker daemon is available, build and run with isolated temporary configuration supplied through `configs.file`; verify `/healthz`, blocked-IP behavior, valid/invalid reload and recovery using in-place host-file updates. Separately inspect rename visibility on this backend and ensure README does not promise it. Do not make Telegram requests or change the user's running services. Remove temporary containers afterward. If unavailable, record the unverified Docker runtime step explicitly.
- [ ] Obtain independent whole-change review, address actionable findings, run affected checks and commit the completed result with a Conventional Commit and co-author footer. Do not push or deploy externally.

## Execution recommendation

Use native execution in this chat, then one independent final review. The four tasks share the same policy and request-observation interfaces, so a single implementer avoids coordination overhead; the final reviewer independently checks the IP trust boundary and reload fallback.

## Plan self-review

All user requirements map to Tasks 1–4. Each Review Focus item has an owning test step. `AccessConfig`, `atomic.Pointer[AccessConfig]`, `Config.Access` and middleware APIs are consistent across tasks. Product implementation remains pending execution-method selection and plan review under `superpowers:writing-plans`, plus explicit approval for the minimum application-specific integration described in the spec. Any further adapter or workaround must be discussed before implementation.
