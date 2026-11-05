FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tg-api-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tg-api-proxy /tg-api-proxy
EXPOSE 8080
USER 65534:65534
ENTRYPOINT ["/tg-api-proxy"]
