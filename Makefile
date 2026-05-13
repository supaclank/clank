# ---- Build / install -------------------------------------------------

# Path of the cross-compiled clank-host binary embedded into the
# Sprites provisioner. .gitignored; rebuilt by `make embed-host`.
# Path matches the //go:embed directive in pkg/provisioner/flyio/embed.go;
# changing this requires updating that directive in lockstep.
EMBED_HOST_BIN := pkg/provisioner/flyio/clank-host-linux-amd64

.PHONY: install
install: embed-host
	go install ./cmd/clank/ ./cmd/clankd/ ./cmd/clank-host/

.PHONY: test test-race
test: embed-host
	go test ./...

test-race: embed-host
	go test -race ./...

# ---- Code generation -------------------------------------------------
#
# Regenerates sqlc-derived code (internal/store/sqlitedb/*) from the
# schema and queries under internal/store/{schema,queries}. Run after
# editing either, then commit the generated files alongside.
#
# Requires sqlc on PATH (`brew install sqlc` on macOS).

.PHONY: generate
generate:
	sqlc generate -f internal/store/sqlc.yaml

# ---- Embedded clank-host (Sprites host bootstrap) --------------------
#
# Cross-compiles cmd/clank-host for linux/amd64 into the path that
# internal/provisioner/flyio/embed.go expects via //go:embed. The
# Sprites provisioner pushes this binary into a sprite via the SDK's
# filesystem API and registers it as a service.
#
# Pure-Go (CGO=0) so the cross-compile works on any host without
# needing a linux toolchain. -trimpath strips local filesystem paths
# from the binary for reproducibility.

.PHONY: embed-host
embed-host:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
	    -trimpath -o $(EMBED_HOST_BIN) \
	    ./cmd/clank-host

# ---- clank-host sandbox image ----------------------------------------
#
# Used by the cloud hub's Daytona launcher. Daytona pulls this image
# from a public registry, so `image-push` is the loop you'll run when
# iterating on the sandbox bootstrap.
#
# Defaults publish to ghcr.io/acksell/clank-host:dev. Override at the
# command line for a personal namespace, e.g.:
#
#   make image-push IMAGE_REPO=axelengstrom/clank-host IMAGE_TAG=mytest
#
# IMPORTANT: ghcr.io images are private by default. After the first
# push, set the package visibility to public on github.com/acksell —
# Daytona pulls anonymously.

IMAGE_REGISTRY ?= ghcr.io
IMAGE_REPO     ?= acksell/clank-host
IMAGE_TAG      ?= dev
IMAGE          := $(IMAGE_REGISTRY)/$(IMAGE_REPO):$(IMAGE_TAG)

# Force amd64 — Daytona runs on x86 hosts; building on Apple Silicon
# without --platform produces an arm64 image that fails to pull.
IMAGE_PLATFORM ?= linux/amd64

.PHONY: image image-push image-print

image:
	docker buildx build \
		--platform $(IMAGE_PLATFORM) \
		--load \
		-f cmd/clank-host/Dockerfile \
		-t $(IMAGE) \
		.

image-push: image
    # compatibile with podman (buildx with --push didn't work)
	docker push $(IMAGE) 

image-print:
	@echo $(IMAGE)

# ---- Self-hosted docker stack (smoke testing) ------------------------
#
# Brings up minio + clank-sync + clankd in containers. See
# docker/README.md for the full smoke recipe (register worktree, push
# checkpoint, trigger migration).

.PHONY: dev dev-rebuild
# One-command local dev: spawn a Cloudflare quick tunnel pointing at
# minio, write the URL into docker/.env, bring the stack up. Foreground;
# ctrl-c tears down both. See scripts/dev.sh for details.
dev:
	@bash scripts/dev.sh

# Like `make dev` but rebuilds the docker image from scratch first.
# Use when a code change isn't picking up — podman/docker layer cache
# can occasionally hold a stale `go build` layer when the surrounding
# context didn't visibly change.
dev-rebuild:
	docker compose -f docker/docker-compose.yml build --no-cache
	@bash scripts/dev.sh

.PHONY: docker-setup docker-up docker-down docker-build docker-logs tunnel
docker-setup:
	@if ! grep -q '^[^#]*[[:space:]]clank-minio\b' /etc/hosts; then \
	    echo "Adding 'clank-minio' → 127.0.0.1 to /etc/hosts (sudo prompt)..."; \
	    echo "127.0.0.1 clank-minio" | sudo tee -a /etc/hosts > /dev/null; \
	else \
	    echo "/etc/hosts already maps clank-minio — no change."; \
	fi
docker-build:
	docker compose -f docker/docker-compose.yml build
docker-up: docker-setup
	docker compose --env-file docker/.env -f docker/docker-compose.yml up -d --build 2>/dev/null \
	  || docker compose -f docker/docker-compose.yml up -d --build
docker-down:
	docker compose -f docker/docker-compose.yml down
docker-logs:
	docker compose -f docker/docker-compose.yml logs -f --tail=100

# Opens a Cloudflare quick tunnel pointing at the local minio port
# (9000 by default). Print the trycloudflare URL and exit — copy it
# into docker/.env as CLANK_SYNC_S3_ENDPOINT, then `make docker-up`
# to rebuild with public-reachable presigned URLs. Required when
# pushing checkpoints to a sandbox that isn't on the same network as
# minio (e.g. fly.io). Without this the sprite can't resolve
# `clank-minio` and the pull-based migration step fails.
#
# Foreground; ctrl-c to stop. Quick tunnels are anonymous (no Cloudflare
# account needed) and rotate per restart, so re-set CLANK_SYNC_S3_ENDPOINT
# after each rerun.
tunnel:
	@echo "Tunnel will expose http://localhost:$${MINIO_API_PORT:-9000} publicly."
	@echo "Set CLANK_SYNC_S3_ENDPOINT in docker/.env to the URL below, then run 'make docker-up'."
	cloudflared tunnel --url http://localhost:$${MINIO_API_PORT:-9000} --no-autoupdate
