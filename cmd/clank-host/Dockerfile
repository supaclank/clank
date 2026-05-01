# Pre-baked clank-host image for cloud sandboxes (Daytona).
#
# Thin overlay on top of Daytona's own pre-warmed sandbox snapshot, so
# we inherit (and de-duplicate against) the layers Daytona already keeps
# hot: opencode, claude, node, bun, git, ca-certificates, etc. We add
# only the clank-host Go binary and the entrypoint script — the runtime
# layer is now ~tens of MB instead of ~700MB, which makes Daytona's
# cold-pull on first sandbox-spawn fast enough that subsequent spawns
# from the same snapshot start in the ~90ms range Daytona advertises.
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
# Stays on alpine: clank-host is pure-Go (CGO=0), no glibc problems
# here because we ship the binary, not the build environment.
FROM golang:1.25-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/clank-host ./cmd/clank-host

# ---- runtime ---------------------------------------------------------
# Daytona's default snapshot. Pinning to a specific tag (rather than
# :latest) so a Daytona-side image bump doesn't silently break us.
# This image already provides:
#   - git, curl, ca-certificates
#   - node, bun
#   - opencode, claude (the agent CLIs clank-host spawns)
#   - daytona user (uid 1001) which Daytona runs containers as
FROM daytonaio/sandbox:0.6.0

# Install our binary and entrypoint as root, then drop back to the
# default daytona user so the container runs unprivileged at runtime
# (matching Daytona's default snapshot behavior).
USER root
COPY --from=builder /out/clank-host /usr/local/bin/clank-host
COPY cmd/clank-host/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 0755 /usr/local/bin/clank-host /usr/local/bin/entrypoint.sh
USER daytona

# Conventional default; the cloud hub's Daytona launcher passes
# CLANK_HOST_PORT=7878 explicitly. EXPOSE matches the launcher's
# HostPort constant.
ENV CLANK_HOST_PORT=7878
EXPOSE 7878

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
