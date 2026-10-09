# Multi-stage, multi-arch-capable build (linux/amd64, linux/arm64).
#
# Recommended: build each arch natively on its own host and join with
# `docker buildx imagetools create` (this is what .circleci/config.yml does).
# Cross-building a single image with `buildx --platform amd64,arm64` from an
# amd64 host also works but runs the arm64 leg under QEMU, which is ~10x
# slower (the Go compile alone takes ~10 min emulated).
#
# Local single-arch build (native):
#   docker build -t plurality .
#
# Local cross-arch build (slow on non-native legs):
#   docker buildx build --platform linux/amd64,linux/arm64 -t <tag> --push .
#
# The Flutter stage is pinned to $BUILDPLATFORM because its output (web
# assets) is arch-independent — no point emulating the toolchain.

# Stage 1: Build the Flutter web app
FROM --platform=$BUILDPLATFORM dart:stable AS flutter_builder

# Install Flutter
RUN apt-get update && apt-get install -y curl git unzip xz-utils zip libglu1-mesa

# Get Stable Branch
RUN git clone https://github.com/flutter/flutter.git /flutter && \
  git -C /flutter checkout stable
ENV PATH="/flutter/bin:${PATH}"

# Copy the Flutter app source
WORKDIR /app
COPY client/ ./client/
COPY scripts/ ./scripts/

# Sync the bundled version asset with the pubspec version so the web update
# check can't drift into a reload loop (see scripts/version.sh).
RUN sh scripts/version.sh

WORKDIR /app/client


# Build the Flutter web app
RUN flutter pub get
RUN flutter build web --release

# Stage 2: Build the Go server
# Runs on the *target* platform (no --platform override) so CGO links against
# the matching libc/libsqlite3 for that arch. buildx auto-selects the right
# golang:1.25 image variant per platform.
FROM golang:1.25 AS go_builder
ARG TARGETOS
ARG TARGETARCH

# Install build dependencies. libsqlite3-dev provides /usr/include/sqlite3.h, which
# sqlite-vec-go-bindings@v0.1.7-alpha.2 needs at CGO compile time — the Go
# module zip served by proxy.golang.org does not include sqlite3.h (only
# lib.go, sqlite-vec.c, sqlite-vec.h), so the system header is used as a
# fallback. mattn/go-sqlite3 still provides the actual sqlite implementation.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash curl git gcc libc6-dev libsqlite3-dev python3 python3-pip python3-venv && \
    rm -rf /var/lib/apt/lists/*

# Copy the Go app source
WORKDIR /app
COPY server/ ./server/
WORKDIR /app/server

# Build the Go application (build.sh sets its own CGO_CFLAGS).
# GOOS/GOARCH come from buildx's per-target args.
# CACHEBUST: pass --build-arg CACHEBUST=<git rev> so the Go binary is always
# recompiled from the current source (prevents a stale cached 'go build' from
# shipping an old server binary even when the checkout is up to date).
ARG CACHEBUST=latest
RUN chmod +x build.sh

# The RUN command below LITERALLY references ${CACHEBUST} so a changed
# build-arg value changes the RUN instruction's command string and
# therefore INVALIDATES the layer cache. A bare `ARG CACHEBUST` with no
# reference does NOT bust anything (Docker only invalidates from the
# first USE of the arg) -- that was the bug: CI passed CACHEBUST=<sha>
# but the go build layer was served from cache, so the image shipped a
# STALE binary built from an old checkout (observed: `latest` image
# built from #49 still contained the Sep-30 19:28 binary with none of
# the #37-#49 fixes).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    echo "Building Plurality server @ CACHEBUST=${CACHEBUST}" && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} ./build.sh


# Copy litellm requirements for installation in final stage
RUN mkdir -p build/litellm && cp litellm_requirements.txt build/litellm/

# Stage 3: Create the final image
FROM debian:bookworm-slim

# Install runtime dependencies (Python needed for LiteLLM proxy;
# nodejs/npm needed for npx-based MCP servers; git needed for MCP servers
# that clone repos / for go install-style tooling).
# Node 22 LTS via NodeSource — Debian bookworm's packaged nodejs is 18.x,
# which is past end-of-life and too old for several MCP servers.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl gnupg git python3 python3-venv python3-pip tini && \
    mkdir -p /etc/apt/keyrings && \
    curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg && \
    echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_22.x nodistro main" > /etc/apt/sources.list.d/nodesource.list && \
    apt-get update && apt-get install -y --no-install-recommends nodejs && \
    rm -rf /var/lib/apt/lists/*

# Pre-install Playwright MCP server and its Chromium browser with system deps.
# Doing this at build time avoids a multi-hundred-MB download on first MCP call.
RUN npm install -g @playwright/mcp@latest playwright && \
    npx --yes playwright install --with-deps chromium && \
    npm cache clean --force

WORKDIR /app

# Copy the compiled Go binary and litellm files from the builder stage
COPY --from=go_builder /app/server/build/ /app/

# Build LiteLLM venv using runtime Python (avoids glibc version mismatch)
# Stub out pyroscope-io (needs Rust/cargo to build, not needed at runtime)
RUN mkdir -p /tmp/dummy-pyroscope && \
    printf '[project]\nname = "pyroscope-io"\nversion = "99.0.0"\n' > /tmp/dummy-pyroscope/pyproject.toml && \
    echo 'pyroscope-io>=99.0.0' > /tmp/pip-constraints.txt && \
    python3 -m venv /app/litellm/litellm_venv && \
    /app/litellm/litellm_venv/bin/pip install --no-cache-dir /tmp/dummy-pyroscope && \
    PIP_CONSTRAINT=/tmp/pip-constraints.txt \
    /app/litellm/litellm_venv/bin/pip install --no-cache-dir -r /app/litellm/litellm_requirements.txt && \
    rm -rf /tmp/dummy-pyroscope /tmp/pip-constraints.txt

# Copy the Flutter web build to the static directory
RUN mkdir -p /app/web
COPY --from=flutter_builder /app/client/build/web /app/web

# Create directories for volumes
RUN mkdir -p /app/users-data /app/data

# Declare runtime data volumes. These are ALWAYS explicitly mounted by the
# host/compose (see deployment.md) â do NOT add anonymous VOLUME entries here:
# a VOLUME /root (or any dir that holds build tooling / binaries) makes Docker
# create a persistent anonymous volume that SHADOWS that path on every
# container start. A stale anonymous volume survives image rebuilds, so a
# freshly layered binary can be masked by an old one from a previous
# container, and /root tooling (go cache, npx, playwright) silently drifts.
# Data dirs only, declared, not materialized.
VOLUME /app/users-data
VOLUME /app/data

# Expose the port the server listens on
EXPOSE 8090

# Run the server under tini so PID 1 reaps zombies and forwards signals
ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/app/Plurality"]

# Healthcheck for orchestrators / autoheal watchers (e.g. willfarrell/autoheal),
# which restart any container lacking a HEALTHCHECK every cycle. The server
# exposes /health (returns 200 when the HTTP stack is up), so a healthy HTTP
# endpoint is the correct liveness signal. 20s interval, 3 retries, 5s timeout.
HEALTHCHECK --interval=20s --timeout=5s --start-period=30s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8090/health >/dev/null 2>&1 || exit 1
