FROM golang:1.25 AS builder
WORKDIR /build
COPY go.work go.work.sum ./
COPY cmd/aileron/go.mod cmd/aileron/go.mod
COPY cmd/aileron-sh/go.mod cmd/aileron-sh/go.mod
COPY cmd/aileron-mcp/go.mod cmd/aileron-mcp/go.mod
COPY sdk/go/go.mod sdk/go/go.mod
COPY test/integration/go.mod test/integration/go.mod
COPY enclave/ enclave/
COPY core/ core/
COPY cmd/aileron-enclave/ cmd/aileron-enclave/
WORKDIR /build/cmd/aileron-enclave
RUN CGO_ENABLED=0 go build -o /aileron-enclave .

# Minimal runtime image. The Confidential Space VM provides hardware
# isolation around the container — no special base image needed.
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=builder /aileron-enclave /aileron-enclave
ENTRYPOINT ["/aileron-enclave"]
