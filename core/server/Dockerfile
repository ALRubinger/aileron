FROM golang:1.24-alpine AS builder

WORKDIR /src

# Copy module files first for layer caching.
COPY go.work go.work.sum* ./
COPY cmd/aileron-mcp/go.mod ./cmd/aileron-mcp/
COPY core/go.mod core/go.sum ./core/
COPY sdk/go/go.mod sdk/go/go.sum* ./sdk/go/
COPY test/integration/go.mod ./test/integration/

RUN cd core && go mod download

COPY core/ ./core/
COPY sdk/go/ ./sdk/go/

RUN cd core && go build -o /aileron-server ./server

FROM alpine:3.20

RUN addgroup -S aileron && adduser -S aileron -G aileron

# Install Atlas CLI for schema migrations.
RUN apk add --no-cache curl && \
    curl -sSf https://atlasgo.sh | sh && \
    apk del curl

COPY --from=builder /aileron-server /usr/local/bin/aileron-server
COPY core/schema/schema.hcl /schema/schema.hcl
COPY core/server/entrypoint.sh /usr/local/bin/entrypoint.sh

USER aileron

EXPOSE 8080

ENTRYPOINT ["entrypoint.sh"]
