# Pre-baked clank-host image for cloud sandboxes (Daytona).
#
# The cloud hub provisions sandboxes from this image. The entrypoint
# starts clank-host on $CLANK_HOST_PORT, configured to clone repos
# from the cloud hub at $CLANK_HUB_URL using the bearer token in
# $CLANK_HUB_TOKEN.
#
# Build:
#   docker build -f cmd/clank-host/Dockerfile -t ghcr.io/acksell/clank-host:dev .
#
# Run locally (mostly for image smoke-testing — production deploys go
# through Daytona):
#   docker run --rm -p 7878:7878 \
#     -e CLANK_HUB_URL=http://host.docker.internal:7878 \
#     -e CLANK_HUB_TOKEN=tkn \
#     -e CLANK_HOST_PORT=7878 \
#     ghcr.io/acksell/clank-host:dev

# ---- builder ---------------------------------------------------------
FROM golang:1.25-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled for a static binary that runs on the runtime stage.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/clank-host ./cmd/clank-host

# ---- runtime ---------------------------------------------------------
# Use a glibc-based image. opencode ships a Bun-built single binary that
# is *not* statically linked against musl, so it crashes on Alpine with
# "Error relocating ... GLIBC_..." This is a known issue for Bun
# binaries; the cheapest fix is to use Debian slim instead of Alpine.
FROM debian:bookworm-slim

# git:             needed by host.workDirFor for clones from the cloud-hub mirror.
# ca-certificates: needed for HTTPS clones.
# curl, unzip:     needed by the bun installer.
# libstdc++6:      runtime dep of bun-built binaries (opencode, claude-code).
# nodejs:          opencode and @anthropic-ai/claude-code ship CLIs with a
#                  `#!/usr/bin/env node` shebang, so a node interpreter must
#                  be on PATH even though we install the packages via bun.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        curl \
        unzip \
        libstdc++6 \
        nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install bun system-wide, then use it to install both agent CLIs.
# bun global bins land in /root/.bun/bin — symlink them to /usr/local/bin
# so they are on $PATH for any user Daytona runs the entrypoint as.
RUN curl -fsSL https://bun.sh/install | bash \
    && ln -s /root/.bun/bin/bun /usr/local/bin/bun

RUN bun add -g opencode-ai @anthropic-ai/claude-code \
    && ln -s /root/.bun/bin/opencode /usr/local/bin/opencode \
    && ln -s /root/.bun/bin/claude   /usr/local/bin/claude \
    && opencode --version \
    && claude --version

WORKDIR /workspace
COPY --from=builder /out/clank-host /usr/local/bin/clank-host
COPY cmd/clank-host/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Conventional default; the cloud hub's Daytona launcher passes
# CLANK_HOST_PORT=7878 explicitly. The EXPOSE matches the launcher's
# HostPort constant.
ENV CLANK_HOST_PORT=7878
EXPOSE 7878

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
