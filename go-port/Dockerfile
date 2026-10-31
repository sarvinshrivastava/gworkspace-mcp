# gworkspace-mcp Go port — production image.
# Smallest practical footprint that keeps every shipped capability: the
# gateway + admin dashboard + policy/audit SQLite (modernc.org/sqlite, pure
# Go) are one static binary; CA roots and tzdata ride along because outbound
# TLS and log timestamps need them. No shell, package manager or HTTP client
# binary: a container that holds Google credentials and sits on the vault
# network should not ship tooling for whatever gets execution inside it.

FROM golang:1.26.6-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The suite runs here, on the exact toolchain the image ships, so a
# regression fails the build rather than the VPS. CGO off: modernc.org/sqlite
# is pure Go, so the binary is fully static.
RUN CGO_ENABLED=0 go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /out/gworkspace-mcp ./cmd/gworkspace-mcp \
 && mkdir -p /state /data/config

FROM alpine:3.23 AS certs
RUN apk add --no-cache ca-certificates tzdata

FROM scratch
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=certs /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=build /out/gworkspace-mcp /app/gworkspace-mcp
# The token store (a tmpfs at runtime) and the policy database (a named
# volume). A fresh named volume copies ownership from these directories.
COPY --from=build --chown=65534:65534 /state /state
COPY --from=build --chown=65534:65534 /data/config /data/config
USER 65534:65534
EXPOSE 3014 3015
# The binary checks its own /healthz: scratch has no shell, curl or node.
HEALTHCHECK --interval=60s --timeout=10s --start-period=90s --retries=3 \
  CMD ["/app/gworkspace-mcp", "-healthcheck"]
CMD ["/app/gworkspace-mcp"]
