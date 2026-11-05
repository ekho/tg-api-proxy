# Standalone Telegram Bot API proxy

## Intent and agreed scope

Create an independent copy of the existing local Telegram Bot API proxy in this repository. Preserve its streaming, file transfer, long polling, token redaction, structured logging and optional OpenTelemetry. Replace header-key authentication with per-client bot and source-IP authorization. Run through Docker Compose behind an existing reverse proxy.

The user selected a separate YAML configuration file, supplied through Docker Compose `configs` with `file: ./config.yaml`, and requested automatic reload: an invalid update must be logged and must not replace the last valid configuration. The source checkout is read-only; its Git history and platform operations are not imported.

## Approaches

1. Separate YAML file supplied through Compose `configs.file`: readable client lists and comments, independent of container settings. Selected by the user, including the single-file mount limitation described below. A directory bind mount is not the selected approach.
2. Inline configuration in Compose `configs.content`: fewer files, but mixes access policy with container deployment. Not selected.
3. Environment-only client records: avoids a file but is awkward for mixed IP lists and does not support the requested file reload. Rejected.

## Configuration

`config.yaml` contains only the settings that can be reloaded together:

```yaml
trusted_proxies:
  - "172.30.0.10"

clients:
  - name: service-a
    bot_ids: ["123456"]
    allowed_ips:
      - "203.0.113.10"
      - "198.51.100.0/24"
      - "192.0.2.10-192.0.2.30"
      - "2001:db8::/48"
```

- Each `allowed_ips` entry is an IPv4/IPv6 address, CIDR subnet, or inclusive `start-end` range. Entries can be mixed freely. Ranges must have ordered endpoints of the same address family. Scoped IPv6 addresses are rejected.
- Client names are unique, nonempty and limited to `A-Za-z0-9_-`. Each client must have at least one numeric bot ID and at least one IP rule. Empty clients, unknown YAML fields and malformed values are errors, never unrestricted access.
- A request must match both a client's `bot_ids` and `allowed_ips`. Shared bot IDs remain supported. If a request matches multiple clients, reject it rather than assign it by configuration order.
- Missing or invalid initial configuration prevents startup. After startup, read/parse/validation errors retain the complete previous policy.
- `CONFIG_FILE` selects the path; its local default is `config.yaml`, and Compose sets `/config/config.yaml`.
- Process settings remain environment variables: `PORT`, `LONG_POLL_MAX_TIMEOUT`, `LOG_EXCLUDE_METHODS`, and standard `OTEL_*`. They are documented separately and are not advertised as reloadable file settings. Outgoing connections use standard `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`; there is no mandatory platform tunnel.

## Trusted source IP

- By default use the TCP peer address. Ignore all forwarding headers from peers not in `trusted_proxies`.
- For a trusted peer, use the existing `go-chi/chi/v5/middleware.ClientIPFromXFF` algorithm with explicit trusted prefixes. It walks right to left, stops at the first untrusted address, and leaves the client IP unset if a traversed hop is malformed or no client can be identified. Unexamined entries to the left cannot override that result. Other forwarding headers do not authorize requests. Do not add a custom chain parser or stricter interpretation around the library.
- Resolve the address once using the same immutable policy snapshot that authorizes the request. Logs and traces use that resolved address as well.
- The reverse proxy must replace or safely append the peer address to `X-Forwarded-For`, terminate TLS, and restrict access to the application. The trust list should contain exact proxy addresses or a dedicated network, not arbitrary private networks.
- Return `403` when source-IP/client authorization fails; malformed Bot API paths return `400`. `/healthz` remains an unauthenticated liveness endpoint and does not proxy traffic.

## Reload behavior

- Poll file contents every **1 second**; use a standard-library ticker, without a filesystem-watcher dependency or reload endpoint.
- Parse and validate a complete candidate off the request path, then atomically publish one immutable policy. In-flight requests retain their snapshot; subsequent requests use the new one.
- Detect all content changes visible at the configured path, including same-size edits and atomic replacements when the process can see them. Compose supplies the file through `configs` as shown below; do not replace this with a directory mount.
- In ordinary Docker Compose, `configs.file` uses a bind mount of the individual file. In-place writes to the existing host file are visible in the container, but replacement by `rename` can leave the container reading the old file. Document this limitation explicitly: use an update that preserves the existing file, or recreate the container after replacing the file. A restart alone does not promise to refresh the mount. The service cannot reload contents that Docker does not expose to it.
- Log a successful application. Log each distinct invalid candidate or file-read failure once until the condition changes; do not flood logs every second. Continue detecting later repairs and deletions/recreations.
- Shutdown cancels and joins the reload loop. No per-request file reads and no restart for access-policy changes.

## Import and delivery

- Import the Go application, its runtime tests, module manifests and container build. Rename the module to `github.com/ekho/tg-api-proxy`.
- Do not import the old platform metadata, Kubernetes manifests, key issuance/rotation tools, client roster, organization-specific agent skills, historical design documents, or deployment/notification workflows.
- Rewrite the README and examples; add Compose, a safe configuration example, `.dockerignore`, and independent build/test/vet/format CI. No old organization names, domains, contacts or credentials in delivered files.
- Keep dependencies already used by the proxy. Prefer standard Go functionality; where it does not cover the requirement, use a maintained community module. Candidate additions are `github.com/goccy/go-yaml` for strict YAML, `go4.org/netipx` for inclusive ranges and IP sets, and `github.com/go-chi/chi/v5/middleware` for trusted-proxy IP extraction. Use `net/netip` directly for addresses/subnets and `sync/atomic` directly for publication.
- Compose uses a non-root container, a read-only config file supplied by `configs.file`, a stop grace period greater than the 60-second request drain plus 5-second telemetry flush, and a localhost-bound application port for a host reverse proxy. Document joining a shared Docker network with no published port for a container reverse proxy.
- Runtime configuration is ignored by Git and excluded from image builds. The example uses documentation-only IP addresses and synthetic bot IDs.

## Verification and non-goals

Test IP families, boundaries and mixed lists; strict YAML validation; spoofed/repeated forwarding headers and proxy chains; client isolation and ambiguous matches; successful/failed reload and recovery; atomic file replacement visible to the process, and concurrent requests under the race detector. Retain streaming, long polling, observability and token-leak regression tests. Verify Compose syntax and a local container/reload smoke test using in-place host-file updates when the Docker daemon is available; separately verify/document replacement visibility on the tested Docker backend without promising cross-platform rename propagation.

The required Compose configuration structure is:

```yaml
services:
  tg-api-proxy:
    configs:
      - source: proxy_config
        target: /config/config.yaml
configs:
  proxy_config:
    file: ./config.yaml
```

References: [Compose service configs](https://github.com/compose-spec/compose-spec/blob/main/05-services.md#configs) and [single-file bind mounts and inode replacement](https://github.com/moby/moby/issues/6011).

No live Telegram calls, external deployment, UI, database, key management, package publication, or changes to the source checkout. A request already admitted before a policy update is allowed to finish.

## Library-first implementation constraint

The user explicitly requires existing standard/community functionality instead of bespoke implementations. Do not write an IP/range parser, IP-set implementation, forwarded-header algorithm, configuration framework, or abstraction over these packages. Verify maintenance, stable versions and actual API behavior before pinning new dependencies.

Use `netip.ParseAddr` / `netip.ParsePrefix`, `netipx.ParseIPRange` / `IPSetBuilder` / `IPSet.Contains`, strict YAML decoding, and Chi's existing IP middleware directly. Test the application's integration with those APIs, without duplicating their full test suites. Chi middleware can be used with the existing `net/http` handler; adopting the Chi router is not required.

The application-specific work not supplied end-to-end by the checked libraries is: validating the client roster and matching a bot/IP to exactly one client; and read -> parse/validate complete candidate -> atomically publish or retain the old version -> log the outcome. Koanf's file provider has change callbacks, but does not provide this complete domain-validation/publication transaction; using it would still require integration. For the one-file, one-second polling requirement, the proposal is to use `time.Ticker`, `os.ReadFile` and `atomic.Pointer` directly rather than add a configuration framework. This minimum application integration requires the user's explicit approval before product code is written. Any further adapter, workaround or library behavior change requires separate discussion.

Library references: [net/netip](https://pkg.go.dev/net/netip), [netipx](https://pkg.go.dev/go4.org/netipx), [YAML decoder options](https://github.com/goccy/go-yaml/blob/master/option.go), [Chi ClientIPFromXFF](https://pkg.go.dev/github.com/go-chi/chi/v5/middleware#ClientIPFromXFF), [Koanf file provider](https://github.com/knadh/koanf/blob/master/providers/file/file.go).

## Builder choices

The file format, reload interval, fail-closed startup, last-valid-policy fallback and IP trust rules above are fixed. The implementer chooses private Go names and test helpers while retaining the existing code structure, subject to the library-first constraint and approval boundary above. A native implementation followed by independent review is recommended because authorization, logging and reload must share a single policy snapshot.
