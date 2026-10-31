# syntax=docker/dockerfile:1.7

# The VodLabs Recorder daemon with its Web UI, for a server or NAS with no
# desktop. See docs/docker.md.
#
# Base images are pinned by digest; Dependabot refreshes the digests.

# Web UI build. Its output is embedded into the daemon (see web/embed.go), so
# it runs before the Go build. It is the same on every target, so it runs once,
# on the build machine's own platform.
FROM --platform=$BUILDPLATFORM oven/bun:1.3.11@sha256:0733e50325078969732ebe3b15ce4c4be5082f18c4ac1a0f0ca4839c2e4e42a7 AS web
WORKDIR /src
COPY package.json bun.lock ./
# No install scripts: the desktop toolchain's would fetch what this build
# never uses.
RUN bun install --frozen-lockfile --ignore-scripts
COPY . .
RUN bun run web:build

# Cross-compiles natively on the build platform; the binaries are pure Go.
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=docker
ARG COMMIT=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist

# The ffmpeg the binaries embed, for the target. download.sh checks its hash.
# CI mounts a GitHub token so the release lookup isn't rate-limited.
RUN --mount=type=secret,id=github_token \
    GITHUB_TOKEN="$(cat /run/secrets/github_token 2>/dev/null || true)" \
    GOOS=$TARGETOS GOARCH=$TARGETARCH bash internal/ffmpeg/download.sh

# The CLI ships beside the daemon, so `docker compose exec recorder vod …`
# needs no host install and can't drift from the daemon's version.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    for cmd in daemon:vlrecd cli:vod; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
      go build -trimpath -buildvcs=false \
        -ldflags "-s -w -X github.com/vodlabs-io/recorder/internal/version.Version=${VERSION} -X github.com/vodlabs-io/recorder/internal/version.Commit=${COMMIT}" \
        -o /out/${cmd#*:} ./cmd/${cmd%%:*} || exit 1; \
    done

# The runtime stage has no shell, so its folders are made here. /config and
# /recordings are bind mounts in compose.yaml and belong to the default user.
# /cache is a named volume, which copies this folder's mode when created, so
# it is 1777 for any `user:`.
RUN mkdir -p /rootfs/config /rootfs/recordings /rootfs/cache \
    && chown 1000:1000 /rootfs/config /rootfs/recordings \
    && chmod 1777 /rootfs/cache

# The embedded ffmpeg is dynamically linked against glibc 2.38 or later, and
# on amd64 against libstdc++ too; distroless cc-debian13 has both, plus CA
# certificates and tzdata, and no shell or package manager.
FROM gcr.io/distroless/cc-debian13@sha256:d74d144baa124dd9defd01d71872bb56c49d4441dba6153ccf790364f97e64b1

ARG VERSION=docker
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="VodLabs Recorder" \
      org.opencontainers.image.description="Records live streams; the daemon with its Web UI" \
      org.opencontainers.image.source="https://github.com/vodlabs-io/recorder" \
      org.opencontainers.image.url="https://vodlabs.io" \
      org.opencontainers.image.documentation="https://github.com/vodlabs-io/recorder/blob/main/docs/docker.md" \
      org.opencontainers.image.licenses="PolyForm-Noncommercial-1.0.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

COPY --from=builder /out/vlrecd /out/vod /usr/local/bin/
COPY --from=builder /rootfs/ /

# VLREC_CONTAINER switches the daemon to its container defaults: the Web UI on
# every interface, console logs, recordings in /recordings, no auto-start.
# State goes in /config and everything regenerable in /cache; HOME, the XDG
# cache and TMPDIR point there too, for what resolves its folder from them.
ENV VLREC_CONTAINER=1 \
    VLREC_CONFIG_DIR=/config \
    VLREC_CACHE_DIR=/cache \
    HOME=/config \
    XDG_CACHE_HOME=/cache \
    TMPDIR=/cache \
    VLREC_LAUNCHER=docker

# Only the Web UI. The daemon's NATS port stays on loopback in the container:
# it has no authentication of its own.
EXPOSE 8080

USER 1000:1000

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/vod", "health"]

# No CMD: every setting comes from /config/config.toml or the environment, so
# a Settings change sticks, and a `command:` adds flags rather than replacing
# the defaults.
ENTRYPOINT ["/usr/local/bin/vlrecd"]
