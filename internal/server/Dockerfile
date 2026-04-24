FROM golang:1.25-alpine AS builder

WORKDIR /src

# Build outside the workspace so we only need the modules the server imports.
ENV GOWORK=off

# Copy module files first for layer caching.
COPY internal/go.mod internal/go.sum ./internal/
COPY sdk/go/go.mod sdk/go/go.sum* ./sdk/go/

RUN cd internal && go mod download

COPY internal/ ./internal/
COPY sdk/go/ ./sdk/go/

# COVERAGE=true builds with Go -cover instrumentation for integration test coverage.
# The resulting binary writes coverage data to $GOCOVERDIR on exit.
ARG COVERAGE=false
RUN if [ "$COVERAGE" = "true" ]; then \
    cd internal && go build -cover -o /aileron-server ./server; \
  else \
    cd internal && go build -o /aileron-server ./server; \
  fi

FROM alpine:3.20

RUN addgroup -S aileron && adduser -S aileron -G aileron

# Install Atlas CLI for schema migrations.
RUN apk add --no-cache curl && \
    curl -sSf https://atlasgo.sh | sh && \
    apk del curl

COPY --from=builder /aileron-server /usr/local/bin/aileron-server
COPY internal/schema/schema.hcl /schema/schema.hcl
COPY internal/server/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY AILERON.md /app/AILERON.md

# Create output directory for coverage-instrumented builds (used when COVERAGE=true).
RUN mkdir -p /tmp/gocoverdir && chown aileron:aileron /tmp/gocoverdir

USER aileron

WORKDIR /app

EXPOSE 8080

ENTRYPOINT ["entrypoint.sh"]
