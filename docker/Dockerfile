# syntax=docker/dockerfile:1.6
#
# Multi-stage build that produces /usr/local/bin/{clankd,clank-host,
# clank-auth-stub} in a small runtime image. clankd embeds the sync
# server in TCP mode when CLANK_SYNC_S3_* env vars are present.

ARG GO_VERSION=1.26.0

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache deps as a separate layer so source-only edits skip the
# 200MB+ go-mod download every rebuild.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/clankd          ./cmd/clankd     && \
    go build -trimpath -ldflags "-s -w" -o /out/clank-host      ./cmd/clank-host && \
    go build -trimpath -ldflags "-s -w" -o /out/clank-auth-stub ./cmd/clank-auth-stub

FROM debian:bookworm-slim AS runtime

# git:               clank-host applies bundles via `git fetch <bundle>`.
# ca-certificates:   outbound HTTPS (S3, fly.io).
# tini:              PID 1 reaper so signals propagate cleanly to
#                    clank-host subprocesses spawned by the local
#                    provisioner.
# libsqlite3-0:      runtime dep of the cgo sqlite driver.
# nodejs (NodeSource): opencode-ai and @anthropic-ai/claude-code wrap
#                    `#!/usr/bin/env node` scripts at runtime, plus the
#                    preview feature spawns Metro for Expo projects —
#                    SDK 55 requires Node >= 20.19.4. Debian Bookworm's
#                    apt nodejs is v18 which errors on Array.toReversed
#                    inside metro-config, so we install Node 22 LTS from
#                    NodeSource. npm ships with it.
# gnupg:             NodeSource setup script needs gpg to install its
#                    apt signing key.
#
# Debian (glibc) base because opencode-ai's npm package ships only
# glibc binaries via optionalDependencies; alpine/musl can't run them.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates tini libsqlite3-0 \
        wget curl gnupg \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Mirror the sprite install path: bun is the sole writer for the agent
# CLIs, matching pkg/provisioner/flyio's opencodeInstallScript so docker-
# and sprite-side surfaces stay on the same toolchain. Bun's global bin
# dir is $HOME/.bun/bin — put it on PATH so the clank-host subprocess
# resolves `opencode` and `claude` without symlinking.
#
# HOME is set explicitly because debian:bookworm-slim doesn't export it
# to non-login RUN shells, and bun resolves ~/.bun off $HOME.
#
# opencode-ai:               OpenCode backend CLI, spawned per session
#                            by the host's BackendManager.
# @anthropic-ai/claude-code: `claude` CLI used by the claude-code
#                            backend and the `claude setup-token` PTY
#                            relay in internal/host (auth_anthropic_setup_token.go).
ENV HOME=/root
ENV PATH="/root/.bun/bin:${PATH}"
# Versions must track agent.PinnedOpencodeVersion and
# agent.PinnedClaudeVersion (internal/agent/{opencode,claude}_compat.go)
# so the docker stack runs the same CLIs the fly provisioner pins onto
# sprites — an unpinned install here drifts to "latest at image build",
# the exact failure mode the pins exist to prevent.
RUN npm install -g bun \
    && bun install -g opencode-ai@1.17.18 @anthropic-ai/claude-code@2.1.201 \
    && opencode --version \
    && claude --version

# Persistent state. clankd writes its embedded sync sqlite db here when
# the gateway is in TCP mode with CLANK_SYNC_S3_* configured; its local
# provisioner stores host.db here too. Mount as a named volume in
# docker-compose so it survives container rebuilds.
RUN mkdir -p /var/lib/clank /root/.clank

COPY --from=builder /out/clankd          /usr/local/bin/clankd
COPY --from=builder /out/clank-host      /usr/local/bin/clank-host
COPY --from=builder /out/clank-auth-stub /usr/local/bin/clank-auth-stub

# clankd's local provisioner does exec.LookPath("clank-host"); having
# it on PATH satisfies that without an explicit BinPath option.

ENTRYPOINT ["/usr/bin/tini", "--"]
# Override per-service in docker-compose.yml.
CMD ["clankd", "start", "--foreground"]
