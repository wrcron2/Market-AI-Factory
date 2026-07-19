FROM golang:1.24-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o factory-server ./cmd/server

FROM alpine:3.19
RUN apk add --no-cache ca-certificates sqlite tzdata docker-cli git curl

# Alpine's docker-cli-compose package lags the host's Docker Engine: Oracle's
# host was upgraded to Engine 29.x (min API 1.44), but Alpine 3.19's compose
# package still speaks API 1.43 and gets rejected outright ("client version
# 1.43 is too old"). Install the official compose v2 binary directly instead
# of relying on the distro package, with checksum verification since it's
# pulled into an image that will hold Alpaca secrets.
RUN set -eu; \
    ARCH=$(uname -m); \
    case "$ARCH" in \
      x86_64)  COMPOSE_ARCH=x86_64 ;; \
      aarch64) COMPOSE_ARCH=aarch64 ;; \
      *) echo "unsupported arch: $ARCH" >&2; exit 1 ;; \
    esac; \
    BASE="https://github.com/docker/compose/releases/download/v5.3.1"; \
    cd /tmp; \
    curl -fsSL -o "docker-compose-linux-${COMPOSE_ARCH}" "$BASE/docker-compose-linux-${COMPOSE_ARCH}"; \
    curl -fsSL -o compose.sha256 "$BASE/docker-compose-linux-${COMPOSE_ARCH}.sha256"; \
    sha256sum -c compose.sha256; \
    mv "docker-compose-linux-${COMPOSE_ARCH}" /usr/local/bin/docker-compose; \
    chmod +x /usr/local/bin/docker-compose; \
    rm compose.sha256

WORKDIR /app
COPY --from=builder /app/factory-server .
EXPOSE 9080
CMD ["./factory-server"]
