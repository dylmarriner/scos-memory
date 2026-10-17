# syntax=docker/dockerfile:1.7
#
# One image for the whole fabric.
#
# This replaces nineteen near-identical per-service Dockerfiles. Each of those
# ran its own `go mod download` and its own compile of the shared packages and
# the standard library, then produced its own alpine layer -- nineteen times the
# build work and nineteen image manifests, for nineteen binaries that share
# essentially all of their dependencies. On four Cortex-A76 cores that
# duplication dominated deploy time.
#
# Here the module graph is resolved once and every binary is compiled in a
# single pass that reuses one build cache, into one runtime image. Services
# select their binary with `command:` in docker-compose.yml rather than by
# having a distinct image, so the ~15 MB of Go binaries and the alpine base are
# stored and pulled once for the whole stack instead of per service.
#
# Adding a service means adding its name to SERVICES below and giving it a
# `command:` in compose -- nothing else.

# Pinned to a specific patch, not a floating "1.26-alpine" tag: go.mod pins
# `toolchain go1.26.6`, and the build should always reproduce with the same
# toolchain rather than whatever patch Docker Hub resolves the tag to on a
# given day.
FROM golang:1.27.2-alpine AS build
# CGO_ENABLED=1: code-intelligence's Tree-sitter parsers (tree-sitter-go,
# -javascript, -python, -rust, -typescript) are cgo bindings around the C
# tree-sitter runtime -- built with CGO disabled their Go files are excluded
# by build constraints entirely, which fails the shared build for every
# service, not just code-intelligence. gcc/musl-dev below give the alpine
# build stage the C toolchain cgo needs.
ENV GOFLAGS=-buildvcs=false CGO_ENABLED=1
RUN apk add --no-cache gcc musl-dev
WORKDIR /src

# Resolve modules before the source is copied so editing a handler does not
# invalidate the download layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    go mod download

COPY packages ./packages
COPY services ./services

# Every service's entrypoint package is <service>/cmd/server, so `go build -o`
# into a directory would name all nineteen outputs "server" and collide. Build
# them one at a time into explicitly named outputs, in a single layer that
# shares one warm build cache across all of them.
ARG SERVICES="agent-registry autonomous-control-plane backup-manager \
code-intelligence compatibility-gateway context-optimisation credential-vault execution-proxy \
cloud-control-plane extension-manager graphify-provider hermes-provider mcp-gateway memory-gateway \
memory-intelligence observability okf-synchroniser recovery-orchestrator \
retrieval-orchestrator security-gate tool-governance transparency-projection"

# Stamped into packages/release's SourceSHA/BuildTime via -X below, so every
# running container can report exactly which commit it was built from at
# GET /health/version -- the auto-deploy script checks this against the
# commit it just deployed instead of trusting that `up -d --force-recreate`
# actually landed the new image everywhere (see auto-deploy.sh's history:
# it didn't, once, for six services, for 42+ hours). Left as "unknown" when
# not supplied, which is the correct default for a local `docker build`
# without --build-arg.
ARG SOURCE_SHA=unknown
ARG BUILD_TIME=unknown
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    set -eu; \
    release_pkg="github.com/dylmarriner/scos-memory/packages/release"; \
    ldflags="-s -w -X $release_pkg.SourceSHA=$SOURCE_SHA -X $release_pkg.BuildTime=$BUILD_TIME"; \
    for svc in $SERVICES; do \
      echo "building $svc"; \
      go build -trimpath -ldflags="$ldflags" -o "/out/$svc" "./services/$svc/cmd/server"; \
    done

# bws, the Bitwarden Secrets Manager CLI: backup-manager's secret-manager
# recovery drill reads the backup key back from Bitwarden with it. Pinned to
# a release, and checked against that release's published SHA-256 digests.
FROM alpine:3.24 AS bws
ARG TARGETARCH
ARG BWS_VERSION=2.1.0
RUN apk add --no-cache curl unzip \
    && case "$TARGETARCH" in \
        amd64) arch=x86_64; sum=f59ee150e42b82128d437087e9bac920053c6bfddcb960d20ce9386e5ac9bba6 ;; \
        arm64) arch=aarch64; sum=eb0f1ae61d1c3b74244d2841233276e05c77e8be4da197ed90fc6248387005e1 ;; \
        *) echo "no bws build for $TARGETARCH" >&2; exit 1 ;; \
       esac \
    && curl -fsSLo /tmp/bws.zip "https://github.com/bitwarden/sdk-sm/releases/download/bws-v${BWS_VERSION}/bws-${arch}-unknown-linux-musl-${BWS_VERSION}.zip" \
    && echo "$sum  /tmp/bws.zip" | sha256sum -c - \
    && unzip -o /tmp/bws.zip -d /tmp/bws \
    && install -m 0755 /tmp/bws/bws /usr/local/bin/bws

FROM alpine:3.24
# graphify-provider execs the `graphify` CLI (PyPI `graphifyy`) as a
# subprocess -- python3/pipx are added here, once, for the whole shared
# image, rather than giving that one service its own base image.
# PIPX_BIN_DIR=/usr/local/bin (already on PATH, already where the Go
# binaries land below) makes the installed `graphify` script reachable by
# every user in the image, not just whichever account ran the build.
ENV PIPX_HOME=/opt/pipx PIPX_BIN_DIR=/usr/local/bin
# backup-manager execs pg_dump, pg_restore, createdb and dropdb; the client
# major version matches the pgvector/pgvector:pg17 server so dumps load.
# git is what backup-manager mirrors the OKF repository with. The postgresql17
# server runs only inside backup-manager's point-in-time recovery drill, as a
# scratch cluster on a private socket that replays archived WAL.
RUN apk add --no-cache ca-certificates python3 py3-pip pipx postgresql17-client postgresql17 git \
    && pipx install graphifyy \
    # pip is only needed to install graphify. Every pip release vendors its
    # own urllib3/msgpack/setuptools copies, which carry HIGH CVEs, so pip
    # and pipx leave the runtime image once graphify is in place; nothing
    # in the services runs pip.
    && rm -rf /opt/pipx/shared \
    && apk del --no-cache pipx py3-pip \
    && rm -rf /usr/lib/python3*/site-packages/pip /usr/lib/python3*/ensurepip/_bundled \
    && test -x /usr/local/bin/graphify \
    && addgroup -S scos && adduser -S -G scos scos \
    # State directories for the services that keep local state. compose mounts
    # a named volume over each of these; Docker seeds a fresh named volume from
    # the image path, so creating them here as scos is what makes the volume
    # writable by the unprivileged user the services actually run as.
    && mkdir -p \
        /var/lib/scos/agent-registry/state \
        /var/lib/scos/backups \
        /var/lib/scos/code-intelligence \
        /var/lib/scos/context \
        /var/lib/scos/control-plane \
        /var/lib/scos/execution-proxy/raw \
        /var/lib/scos/execution-proxy/state \
        /var/lib/scos/observability \
        /var/lib/scos/okf \
        /var/lib/scos/wal-archive \
        # graphify-provider's per-repo AST cache (compose mounts the
        # graphify-cache volume here); without it the volume is seeded
        # root-owned and every graphify query fails creating its cache.
        /var/lib/graphify-cache \
    && chown -R scos:scos /var/lib/scos /var/lib/graphify-cache

COPY --from=build /out/ /usr/local/bin/
COPY --from=bws /usr/local/bin/bws /usr/local/bin/bws

# transparency-projection serves this dashboard from disk.
COPY dashboard/index.html /usr/local/share/scos/dashboard/index.html

# findOKFRoot() (services/okf-synchroniser/cmd/server/main.go) resolves the
# relative "okf" candidate against the container's working directory ("/"), and
# readinessCheck requires that directory to be writable. A host bind mount would
# keep host ownership rather than the scos user, so the approved OKF content is
# baked into the image instead -- GOVERNANCE.md section 26: Git remains
# authoritative for approved content; this baked copy is what gets indexed and
# synced from, refreshed on each image rebuild.
COPY okf /okf
RUN chown -R scos:scos /okf

USER scos
WORKDIR /

# No ENTRYPOINT: each compose service names its binary in `command:`, and
# /usr/local/bin is on PATH. Default to a listing so a bare `docker run` of this
# image is self-describing rather than silently doing nothing.
CMD ["sh", "-c", "echo 'scos-fabric image. Run one of:'; ls /usr/local/bin"]
