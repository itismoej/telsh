# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Copy module definition first; go mod tidy will fetch dependencies and
# generate go.sum inside the container (no network access needed on the host).
COPY go.mod .
COPY *.go .

RUN go mod tidy \
 && go build -ldflags="-s -w" -o telsh .

# ── Runtime stage ─────────────────────────────────────────────────────────────
FROM alpine:3.23

# bash is the default shell; install common utilities that make remote work easier.
RUN apk add --no-cache \
    bash \
    curl \
    wget \
    git \
    coreutils \
    procps \
    util-linux

WORKDIR /root

COPY --from=builder /build/telsh /usr/local/bin/telsh

ENTRYPOINT ["/usr/local/bin/telsh"]
