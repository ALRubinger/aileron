FROM golang:1.24-alpine AS builder

WORKDIR /src

# Copy module files first for layer caching.
COPY go.work go.work.sum* ./
COPY cmd/aileron-mcp/go.mod ./cmd/aileron-mcp/
COPY core/go.mod core/go.sum* ./core/
COPY sdk/go/go.mod sdk/go/go.sum* ./sdk/go/
COPY test/integration/go.mod ./test/integration/

RUN go work sync

COPY core/ ./core/
COPY sdk/go/ ./sdk/go/

RUN go build -o /aileron-server ./core/server

FROM alpine:3.20

RUN addgroup -S aileron && adduser -S aileron -G aileron
USER aileron

COPY --from=builder /aileron-server /usr/local/bin/aileron-server

EXPOSE 8080

ENTRYPOINT ["aileron-server"]
