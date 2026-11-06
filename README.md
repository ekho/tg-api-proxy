# tg-api-proxy

[Русский](README.ru.md) | [Detailed documentation](docs/guide.md)

A transparent Telegram Bot API proxy in Go with optional per-client IP rules and
header authentication. Supports regular API calls, file transfers, and long
polling. Bot tokens are passed in the URL as with Telegram and redacted in logs.

## Configure and run

Uses `ghcr.io/ekho/tg-api-proxy:latest`. Copy and edit the configuration:

```sh
cp config.example.yaml config.yaml
```

Example `config.yaml`; replace the bot ID and addresses with your own:

```yaml
trusted_proxies: ["172.30.0.10"]
clients:
  - name: service-a
    bot_ids: ["123456"]
    # auth: {type: bearer, token: "<generated token>"}
    allowed_ips:
      - "203.0.113.10"
      - "198.51.100.0/24"
      - "192.0.2.10-192.0.2.30"
      - "2001:db8::/48"
```

Omit `allowed_ips` for a client whose configured bots can be used from any IP.
Set `auth` to require a header key, a Bearer token, or Basic authentication.
For the Bearer example, generate a token with `openssl rand -hex 32` and send it
in `Authorization: Bearer <token>`. Omit `auth` for a client without credentials.

`trusted_proxies` must contain the reverse proxy's actual source IP. With Nginx
on the host, the app may see a Docker gateway address; with Traefik, use its
address on the shared Docker network. The first request log shows `remote_addr`.

```sh
docker compose pull
docker compose up -d
docker compose logs -f tg-api-proxy
```

Set the bot client's API base URL to `https://proxy.example.com`; request paths
remain `/bot<TOKEN>/<method>` and `/file/bot<TOKEN>/<file_path>`.

## Nginx example

For Nginx on the Docker host. Replace the domain and TLS certificate paths:

```nginx
server {
    listen 443 ssl;
    server_name proxy.example.com;
    ssl_certificate /etc/nginx/certs/fullchain.pem;
    ssl_certificate_key /etc/nginx/certs/privkey.pem;
    client_max_body_size 256m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 75s;
    }
}
```

```sh
sudo nginx -t
sudo nginx -s reload
```

## Traefik example

For an existing Traefik with the Docker provider, shared network `proxy`, HTTPS
entry point `websecure`, and certificate resolver `letsencrypt`. Save this as
`compose.traefik.yaml` and replace the domain:

```yaml
services:
  tg-api-proxy:
    ports: !reset []
    networks: [proxy]
    labels:
      traefik.enable: "true"
      traefik.docker.network: proxy
      traefik.http.routers.tg-api-proxy.rule: Host(`proxy.example.com`)
      traefik.http.routers.tg-api-proxy.entrypoints: websecure
      traefik.http.routers.tg-api-proxy.tls: "true"
      traefik.http.routers.tg-api-proxy.tls.certresolver: letsencrypt
      traefik.http.routers.tg-api-proxy.service: tg-api-proxy
      traefik.http.services.tg-api-proxy.loadbalancer.server.port: "8080"

networks:
  proxy:
    external: true
    name: proxy
```

```sh
docker compose -f compose.yaml -f compose.traefik.yaml pull
docker compose -f compose.yaml -f compose.traefik.yaml up -d
```
