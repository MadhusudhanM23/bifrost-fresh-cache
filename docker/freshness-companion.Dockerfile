# Builds both freshness-companion binaries: the proxy itself (main.go) and the
# tiny demoserver (cmd/demoserver) that serves demo-page/ for the offline/DirectFetch
# demo. One image, two entrypoints selected via `command:` in docker-compose.yml.
# Build context: freshness-companion/ itself.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0
RUN go build -o /out/freshness-companion .
RUN go build -o /out/demoserver ./cmd/demoserver

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/freshness-companion /usr/local/bin/freshness-companion
COPY --from=build /out/demoserver /usr/local/bin/demoserver
WORKDIR /app
EXPOSE 8090 8000
ENTRYPOINT []
CMD ["/usr/local/bin/freshness-companion"]
