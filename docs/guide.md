# Configuration and operations

[Русский](guide.ru.md) | [Quick start](../README.md)

## Running with Docker Compose

```sh
cp config.example.yaml config.yaml
# Set your clients, bot IDs, allowed IPs, and trusted proxy addresses.
docker compose pull
docker compose up -d
docker compose logs -f tg-api-proxy
```

`config.yaml` is required and is excluded from Git and the image. The example uses
synthetic bot IDs and IP addresses reserved for documentation: replace them before
starting the service. An invalid initial configuration prevents startup.

Compose supplies the file through its standard `configs` mechanism:

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

By default, the HTTP port is published only on `127.0.0.1:8080`. Put your HTTPS
reverse proxy in front of it. The container runs as a non-root user with a
read-only filesystem and configuration.

## Published images

GitHub Actions builds and publishes `ghcr.io/ekho/tg-api-proxy` after the CI checks
pass. Images support `linux/amd64` and `linux/arm64`.

| Trigger | Image tags |
|---|---|
| Push to `main` | `latest`, `sha-<full commit SHA>` |
| Push a `v*` Git tag, such as `v1.2.3` | `v1.2.3`, `sha-<full commit SHA>` |
| Manual run (`workflow_dispatch`) | SHA tag; `latest` only when running from `main` |

Pull requests run checks without publishing. Version tags do not update `latest`.
The publishing job uses the built-in `GITHUB_TOKEN` with `packages: write`; no
additional repository secret is required. Docker Actions are pinned to commit
SHAs. The image is published only when the workflow runs successfully on GitHub.

`compose.yaml` uses `ghcr.io/ekho/tg-api-proxy:latest` by default. Run
`docker compose pull` before `docker compose up -d` to download the current image.
To reference a particular revision, change the image tag in Compose to a version
or SHA tag. If the package is private, authenticate Docker to GHCR before pulling.
The first image becomes available only after a successful publication workflow.

For a local container build, run `docker build -t tg-api-proxy:dev .` and set
`image: tg-api-proxy:dev` in Compose. Keep the remaining settings and `configs`.

## Client rules

```yaml
trusted_proxies:
  - "172.30.0.10"
clients:
  - name: service-a
    bot_ids: ["123456", "654321"]
    # auth: {type: header, header: X-API-Key, value: "<generated key>"}
    allowed_ips:
      - "203.0.113.10"
      - "198.51.100.0/24"
      - "192.0.2.10-192.0.2.30"
      - "2001:db8::/48"
```

- `name`: a unique name containing Latin letters, digits, `_`, and `-`; used in
  logs, metrics, and traces.
- `bot_ids`: a nonempty list of numeric identifiers—the part of each token before
  `:`. Do not put the secret part of the token in the configuration.
- Omit `allowed_ips` to allow any source IP for the client's `bot_ids`. If you
  set the field, provide a nonempty list of IPv4 or IPv6 addresses, CIDR subnets,
  or inclusive `start-end` ranges. Both `[]` and `null` are configuration errors.
- Set `auth` to require credentials from this client. Omit both `auth` and
  `proxy_key` for access without credentials. An explicit `auth: null` is invalid.
- `trusted_proxies`: addresses, subnets, or ranges of trusted intermediate proxies.
  An empty or omitted list means forwarding headers are ignored.

A request must match exactly one client by `bot_id` and meet any IP and credential
requirements for that client. Clients can share a bot, so a separate rule without
a key may also allow that bot. A request matching multiple clients, an unlisted
bot, or a disallowed IP receives `403`. If the bot and IP match a rule that
requires credentials and no rule allows the request, a missing, invalid, or
repeated credential header returns `401` with a `WWW-Authenticate` challenge.
An invalid API path returns `400`.
`GET /healthz` returns `200` without access checks and serves a local response.

Unknown YAML fields, duplicate names, empty required lists, and invalid IPs are
configuration errors. IPv6 interface identifiers (`%eth0`) are not supported.
Use native IPv4 notation such as `192.0.2.0/24` for IPv4 subnets, rather than
IPv4-mapped IPv6 notation. An IPv4-mapped connection address
(`::ffff:192.0.2.10`) is matched against its native IPv4 equivalent.

### Authentication

Each client can select one scheme. Use HTTPS and quote credential values in the
private `config.yaml` so YAML reads them as strings. `openssl rand -hex 32`
generates a value usable in all three schemes.

| `auth.type` | Other fields in `auth` | Request credentials |
|---|---|---|
| `header` | `header`, `value` | The configured header and its exact value, for example `X-API-Key` |
| `bearer` | `token` | `Authorization: Bearer <token>` with the configured static token |
| `basic` | `username`, `password` | HTTP Basic credentials in `Authorization` |

Use distinct credentials for different clients. The `header` scheme reserves
HTTP routing, framing, forwarding, tracing, and diagnostic header names.
`proxy_key` is shorthand for a key in `X-Proxy-Key`. It requires at least 16
characters from `A-Za-z0-9_-` and cannot be combined with `auth` on one client.
The proxy strips `Authorization`, `Proxy-Authorization`, `X-Proxy-Key`, and all
custom credential headers in the active policy before forwarding to Telegram.
Changes to credentials and header names apply when the configuration reloads.

## Automatic reload

The service checks the file contents once per second. Each new version is fully
parsed and validated, then replaces all client rules and trusted proxies in one
atomic update. No process restart is required.

If reading, YAML parsing, or rule validation fails, the service logs the error in
JSON and continues using the **last valid configuration**. An unchanged invalid
version does not produce another error every second. The file is checked again
when corrected. Successful updates are logged as `config applied`.
Requests already admitted finish under the previous rules; new requests use the
updated rules.

**A limitation of `configs.file` in ordinary Docker Compose:** it mounts a single
file. Changes to the contents of the existing file are visible inside the
container. Some editors save by creating a new file and replacing the original
with `rename`; the container may then keep reading the old file. The service
cannot see contents that Docker does not expose. Save without replacing the file,
or recreate the container after such a replacement:

```sh
docker compose up -d --force-recreate tg-api-proxy
```

A regular `restart` does not guarantee that the mount is refreshed. This is a
limitation of [single-file bind mounts](https://github.com/moby/moby/issues/6011);
the service still applies validated configuration atomically.
When a file is written in parts, the watcher may observe intermediate contents:
an invalid version is rejected, and the next valid version is applied.

## Reverse proxy configuration

`X-Forwarded-For` is considered **only when the TCP peer is trusted**.
Chi's middleware walks the chain from right to left, skipping explicitly trusted
addresses. The first untrusted address becomes the client IP. A malformed entry
in the examined part of the chain or a chain with no identifiable client leaves
the client IP unknown. Such a request can only match a client that omits
`allowed_ips`. Entries to the left of the identified client cannot override it.
`X-Real-IP` and other headers do not grant access.

For Nginx accepting external connections and terminating TLS:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 75s;
}
```

Set `trusted_proxies` to the address from which the application actually sees the
Nginx connection. With a published Docker port, this may be the Docker gateway
address rather than `127.0.0.1`. The initial diagnostic log for the first request
shows the TCP peer in `remote_addr`. Do not trust all private networks: specify
exact proxy addresses or a dedicated subnet.

If Nginx/Traefik runs in a container, connect the service to a shared Docker
network and remove `ports` from Compose. The reverse proxy connects to
`http://tg-api-proxy:8080`, and `trusted_proxies` contains its address on that
network. The reverse proxy must overwrite the incoming `X-Forwarded-For` header
or correctly append its peer's address.

### Traefik example prerequisites

The quick-start overlay targets an existing Traefik instance with its Docker
provider enabled, the external Docker network `proxy`, entry point `websecure`,
and certificate resolver `letsencrypt`. Replace these names to match your setup.
Traefik and the application must share that network; `traefik.docker.network`
refers to the actual Docker network name. The `ports: !reset []` override removes
the base file's published port. No separate host port is needed for this route.

Set the application's `trusted_proxies` to Traefik's actual address on that
network, not the address of the application container. The application's first
request diagnostic logs the TCP peer as `remote_addr`.
If Traefik itself sits behind another proxy, configure its trusted forwarding
addresses explicitly; do not enable `forwardedHeaders.insecure` for this setup.

The Nginx quick-start example uses a 256 MiB request-body limit. Adjust that
example limit for your deployment; it does not change Telegram's own limits.

References: [Traefik Docker labels](https://doc.traefik.io/traefik/reference/routing-configuration/other-providers/docker/),
[Traefik forwarding trust](https://doc.traefik.io/traefik/reference/install-configuration/entrypoints/),
and [Compose reset overrides](https://docs.docker.com/reference/compose-file/merge/#reset-value).

## Client usage

Regular API call: `https://proxy.example.com/bot<TOKEN>/<method>`.
File download: `https://proxy.example.com/file/bot<TOKEN>/<file_path>`.

Send the configured credentials on API calls and file downloads. For a Bearer
client, `PROXY_TOKEN` holds the configured token.

```sh
curl -H "Authorization: Bearer $PROXY_TOKEN" "https://proxy.example.com/bot${TOKEN}/getMe"
```

For `python-telegram-bot` with a client that uses no credentials:

```python
app = (
    ApplicationBuilder()
    .token(TOKEN)
    .base_url("https://proxy.example.com/bot")
    .base_file_url("https://proxy.example.com/file/bot")
    .build()
)
```

`getUpdates` has a 45-second deadline by default. When this deadline is reached,
the proxy returns `{"ok":true,"result":[]}` with an `X-Proxy-Clamped-Timeout`
header. Other requests and files are streamed without this deadline.

## Process settings and observability

You can create an optional Compose `.env` file from `.env.example`. Environment
settings require container recreation; only the YAML file containing clients and
trusted proxies is reloaded automatically.

| Variable | Default | Purpose |
|---|---|---|
| `CONFIG_FILE` | `config.yaml` | File path for a native run; `/config/config.yaml` in Compose |
| `PORT` | `8080` | Internal HTTP port, from 1024 to 65535 |
| `PROXY_PORT` | `8080` | Compose port published on localhost |
| `LONG_POLL_MAX_TIMEOUT` | `45` | `getUpdates` deadline, in seconds |
| `LOG_EXCLUDE_METHODS` | `getUpdates` | Methods whose successful calls are omitted from the access log; `none` logs all |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` | empty | Standard outgoing connection settings |
| `OTEL_SDK_DISABLED` | `true` in Compose | Disable metrics and trace export |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty | OTLP collector HTTP endpoint when export is enabled |
| `OTEL_RESOURCE_ATTRIBUTES` | empty | Additional OpenTelemetry resource attributes |

Access logs are written to stdout as JSON. Responses with `4xx`/`5xx` status codes
are logged regardless of the method filter. Metrics and traces are also
independent of this filter. To enable OpenTelemetry, set `OTEL_SDK_DISABLED=false`
and your collector endpoint; Compose uses OTLP HTTP/protobuf. An unavailable
collector does not stop the proxy. Without an outgoing proxy, connections to
Telegram are direct.

Addresses and subnets are parsed by the standard `net/netip` package, ranges and
sets by `go4.org/netipx`, YAML by `goccy/go-yaml`, and the trusted proxy chain by
Chi middleware. Application-specific logic is limited to client rules and safely
applying file updates.

## Development

```sh
go test -race ./... -count=1
go vet ./...
go build ./...
OTEL_SDK_DISABLED=true CONFIG_FILE=config.yaml go run .
```

The application does not load `.env` itself. Tests use local HTTP servers; real
bot tokens and calls to Telegram are not required.
