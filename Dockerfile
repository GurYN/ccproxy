# syntax=docker/dockerfile:1.7

# ──────────────────────────────────────────────────────────────────────────
# ccproxy image
#
# IMPORTANT: this image deliberately does NOT include the `claude` binary or
# the user's `~/.claude` config. ccproxy is a thin wrapper around the host's
# Claude Code installation — that's the entire product. Mount both in at
# runtime (see deploy/docker-compose.example.yml).
#
# Build from this directory (the go.mod root):
#   docker build -t ccproxy:dev .
# ──────────────────────────────────────────────────────────────────────────

FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module deps separately from sources for fast incremental builds.
COPY go.mod go.sum ./
RUN go mod download

COPY . ./
ARG VERSION=devel
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/ccproxy ./cmd/ccproxy

# ──────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/guryn/ccproxy"
LABEL org.opencontainers.image.description="OpenAI-compatible HTTP proxy in front of Claude Code"

WORKDIR /app
COPY --from=build /out/ccproxy /usr/local/bin/ccproxy

# Sensible defaults; override at runtime via env or a mounted config.yaml.
# Note: claude binary path, verbosity, effort, model aliases etc. live in
# config.yaml (mount it at $CCPROXY_STATE/config.yaml).
ENV CCPROXY_LISTEN=":4141" \
    CCPROXY_STATE="/var/lib/ccproxy"

EXPOSE 4141
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ccproxy"]
CMD ["serve"]
