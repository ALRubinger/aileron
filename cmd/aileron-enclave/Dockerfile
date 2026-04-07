FROM golang:1.24 AS builder
WORKDIR /build
COPY go.work go.work.sum ./
COPY enclave/ enclave/
COPY core/ core/
COPY cmd/aileron-enclave/ cmd/aileron-enclave/
WORKDIR /build/cmd/aileron-enclave
RUN CGO_ENABLED=0 go build -o /aileron-enclave .

FROM gcr.io/confidential-space-images/confidential-space:latest
COPY --from=builder /aileron-enclave /aileron-enclave
ENTRYPOINT ["/aileron-enclave"]
