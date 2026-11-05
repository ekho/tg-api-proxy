# tg-api-proxy

[English](README.md) | [Подробная документация](docs/guide.ru.md)

Прозрачный Go-прокси Telegram Bot API с доступом по IP для каждого клиента.
Поддерживает обычные вызовы, передачу файлов и long polling. Клиент меняет только
базовый URL API; дополнительный ключ или заголовок авторизации не нужен.
Токены ботов проходят в URL как у Telegram, не сохраняются и скрываются в логах.

## Настройка и запуск

Используется `ghcr.io/ekho/tg-api-proxy:latest`. Скопируйте и заполните конфигурацию:

```sh
cp config.example.yaml config.yaml
```

Пример `config.yaml`; замените идентификатор бота и адреса своими:

```yaml
trusted_proxies: ["172.30.0.10"]
clients:
  - name: service-a
    bot_ids: ["123456"]
    allowed_ips:
      - "203.0.113.10"
      - "198.51.100.0/24"
      - "192.0.2.10-192.0.2.30"
      - "2001:db8::/48"
```

В `trusted_proxies` нужен фактический адрес, с которого приложение видит reverse
proxy. Для Nginx на хосте это может быть шлюз Docker, для Traefik — его адрес
в общей Docker-сети. В логе первого запроса адрес виден в поле `remote_addr`.

```sh
docker compose pull
docker compose up -d
docker compose logs -f tg-api-proxy
```

В клиенте бота укажите базовый адрес API `https://proxy.example.com`; пути остаются
`/bot<TOKEN>/<method>` и `/file/bot<TOKEN>/<file_path>`.

## Пример Nginx

Для Nginx на хосте Docker. Замените домен и пути к TLS-сертификату и ключу:

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

## Пример Traefik

Для существующего Traefik с Docker provider, общей сетью `proxy`, HTTPS-точкой
входа `websecure` и механизмом получения сертификатов `letsencrypt`. Сохраните
в файл `compose.traefik.yaml` и замените домен:

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
