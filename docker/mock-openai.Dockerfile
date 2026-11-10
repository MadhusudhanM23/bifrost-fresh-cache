# Builds tools/mock-openai: a stdlib-only OpenAI-compatible mock server used so the
# demo needs no real API key. Build context: tools/mock-openai/ itself.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0
RUN go build -o /out/mock-openai .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/mock-openai /usr/local/bin/mock-openai
EXPOSE 18080
ENTRYPOINT ["/usr/local/bin/mock-openai"]
CMD ["-host", "0.0.0.0", "-port", "18080", "-dim", "256", "-api-key", "sk-mock-local", "-chat-latency", "100ms"]
