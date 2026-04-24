FROM golang:1.25 AS builder
WORKDIR /build

# Build outside the workspace so we only need the modules the enclave imports.
ENV GOWORK=off

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
ENV AILERON_TEE_PROVIDER=confidential-space
ENV AILERON_ENCLAVE_PORT=8443
EXPOSE 8443
ENTRYPOINT ["/aileron-enclave"]
