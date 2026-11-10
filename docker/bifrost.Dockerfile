# Builds bifrost-http from the LOCAL fork (bifrost/, branch feat/pgvector-vectorstore),
# not from the published github.com/maximhq/bifrost module -- the published module
# does not have framework/vectorstore/pgvector.go.
#
# This mirrors the GOWORK trick in e2e/run-e2e.ps1: a throwaway go.work file points
# transports/bifrost-http (and everything it depends on) at the sibling modules in
# this checkout (core, framework, cli, plugins/*) instead of whatever version of
# those modules go.mod would otherwise resolve from the module proxy.
#
# Build context: bifrost/ (the fork itself), not the repo root -- see
# docker-compose.yml's "context: ./bifrost" and bifrost/.dockerignore, which keeps
# the context to the handful of modules actually needed.
FROM golang:1.27-bookworm AS build
WORKDIR /src

# Copy only the Go modules actually needed to build transports/bifrost-http (per
# e2e/build/go.work's `use` list) -- NOT the whole bifrost/ fork, which also
# contains a >1GB .git history, 600MB of docs, the Next.js dashboard source,
# terraform, etc. The build context for this service is ./bifrost itself (see
# docker-compose.yml), scoped further by bifrost/.dockerignore, so these COPY
# sources are relative to the bifrost/ fork root, not the repo root.
COPY cli/ bifrost/cli/
COPY core/ bifrost/core/
COPY framework/ bifrost/framework/
COPY transports/ bifrost/transports/
COPY plugins/compat/ bifrost/plugins/compat/
COPY plugins/governance/ bifrost/plugins/governance/
COPY plugins/jsonparser/ bifrost/plugins/jsonparser/
COPY plugins/logging/ bifrost/plugins/logging/
COPY plugins/maxim/ bifrost/plugins/maxim/
COPY plugins/mocker/ bifrost/plugins/mocker/
COPY plugins/modelcatalogresolver/ bifrost/plugins/modelcatalogresolver/
COPY plugins/otel/ bifrost/plugins/otel/
COPY plugins/prompts/ bifrost/plugins/prompts/
COPY plugins/routing/ bifrost/plugins/routing/
COPY plugins/semanticcache/ bifrost/plugins/semanticcache/
COPY plugins/telemetry/ bifrost/plugins/telemetry/

# transports/bifrost-http has `//go:embed all:ui`, and ui/ is gitignored (built by a
# separate Node/Next.js step we don't need for this demo). Create a placeholder so
# the embed has something to embed, exactly as e2e/run-e2e.ps1 does for the native build.
RUN mkdir -p bifrost/transports/bifrost-http/ui && \
    if [ ! -s bifrost/transports/bifrost-http/ui/index.html ]; then \
      printf '%s' '<!doctype html><html><head><meta charset="utf-8"><title>Bifrost (placeholder UI)</title></head><body><p>Placeholder UI for a container build. The real dashboard was not built.</p></body></html>' \
        > bifrost/transports/bifrost-http/ui/index.html ; \
    fi

# Out-of-tree go.work: builds transports/bifrost-http against the LOCAL framework/core
# fork in this image instead of the published modules. Equivalent to e2e/build/go.work.
RUN printf 'go 1.27.3\n\nuse (\n\t./bifrost/cli\n\t./bifrost/core\n\t./bifrost/framework\n\t./bifrost/transports\n\t./bifrost/plugins/compat\n\t./bifrost/plugins/governance\n\t./bifrost/plugins/jsonparser\n\t./bifrost/plugins/logging\n\t./bifrost/plugins/maxim\n\t./bifrost/plugins/mocker\n\t./bifrost/plugins/modelcatalogresolver\n\t./bifrost/plugins/otel\n\t./bifrost/plugins/prompts\n\t./bifrost/plugins/routing\n\t./bifrost/plugins/semanticcache\n\t./bifrost/plugins/telemetry\n)\n' > go.work

ENV GOWORK=/src/go.work
ENV CGO_ENABLED=0

WORKDIR /src/bifrost/transports/bifrost-http
RUN go build -ldflags="-X main.Version=v0.0.0-docker-pgvector" -o /out/bifrost-http .

# ---- runtime image -------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/bifrost-http /usr/local/bin/bifrost-http
RUN mkdir -p /app-data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/bifrost-http"]
CMD ["-app-dir", "/app-data", "-port", "8080", "-host", "0.0.0.0", "-log-level", "info", "-log-style", "pretty"]
