FROM golang:1.25-alpine AS builder

WORKDIR /src

# Copy module files first for layer caching.
COPY go.work go.work.sum* ./
COPY cmd/aileron/go.mod ./cmd/aileron/
COPY cmd/aileron-enclave/go.mod ./cmd/aileron-enclave/
COPY cmd/aileron-mcp/go.mod ./cmd/aileron-mcp/
COPY cmd/aileron-sh/go.mod ./cmd/aileron-sh/
COPY core/go.mod core/go.sum ./core/
COPY enclave/go.mod ./enclave/
COPY sdk/go/go.mod sdk/go/go.sum* ./sdk/go/
COPY test/integration/go.mod ./test/integration/

RUN cd core && go mod download

COPY core/ ./core/
COPY enclave/ ./enclave/
COPY sdk/go/ ./sdk/go/

# COVERAGE=true builds with Go -cover instrumentation for integration test coverage.
# The resulting binary writes coverage data to $GOCOVERDIR on exit.
ARG COVERAGE=false
RUN if [ "$COVERAGE" = "true" ]; then \
    cd core && go build -cover -o /aileron-server ./server; \
  else \
    cd core && go build -o /aileron-server ./server; \
  fi

FROM alpine:3.20

RUN addgroup -S aileron && adduser -S aileron -G aileron

# Install Atlas CLI for schema migrations.
RUN apk add --no-cache curl && \
    curl -sSf https://atlasgo.sh | sh && \
    apk del curl

COPY --from=builder /aileron-server /usr/local/bin/aileron-server
COPY core/schema/schema.hcl /schema/schema.hcl
COPY core/server/entrypoint.sh /usr/local/bin/entrypoint.sh

# Create output directory for coverage-instrumented builds (used when COVERAGE=true).
RUN mkdir -p /tmp/gocoverdir && chown aileron:aileron /tmp/gocoverdir

USER aileron

EXPOSE 8080

ENTRYPOINT ["entrypoint.sh"]
