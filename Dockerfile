# ---- сборка ----
FROM golang:1.26-bookworm AS build

WORKDIR /src

# Манифесты зависимостей отдельным слоем: правка кода не сбрасывает кеш.
# Зависимости — OpenTelemetry SDK и его транзитивные зависимости; go.sum уже
# существует, и глоб go.* подхватывает его вместе с go.mod.
COPY go.* ./
RUN go mod download

COPY . .

# CGO выключен — статический бинарник, которому не нужен libc в рантайме.
# -trimpath убирает пути сборочной машины, -s -w срезают отладочные таблицы.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/tg-api-proxy .

# ---- рантайм ----
# distroless/static: нет шелла, пакетного менеджера и тулчейна — только
# корневые сертификаты, нужные для TLS до api.telegram.org.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/tg-api-proxy /tg-api-proxy

# Порт ≥ 1024: процесс работает под непривилегированным UID и занять
# привилегированный порт не может.
EXPOSE 8080

# Числовой UID ≥ 1024 — Pod Security restricted отвергает запуск от root.
USER 65534:65534

ENTRYPOINT ["/tg-api-proxy"]
