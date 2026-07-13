# ---- Build / install -------------------------------------------------

# Path of the cross-compiled clank-host binary embedded into the
# Sprites provisioner. TRACKED in VCS and verified current by CI
# (rebuild + git diff); rebuilt by `make embed-host`. Path matches the
# //go:embed directive in pkg/provisioner/flysprites/embed.go; changing this
# requires updating that directive in lockstep.
EMBED_HOST_BIN := pkg/provisioner/flysprites/clank-host-linux-amd64

.PHONY: install
install: embed-host
	go install ./cmd/clank/ ./cmd/clankd/ ./cmd/clank-host/

# ---- Voice (opt-in) --------------------------------------------------
#
# clank-voice is the local dictation engine for `clank preview`'s
# push-to-talk (see voice-engine/README.md). It's a SEPARATE Go module
# so its cgo sherpa-onnx dependency — and the per-platform onnxruntime
# libs cgo downloads on first build — never touch the main module's
# builds, CI, or the CGO_ENABLED=0 embedded clank-host. Hence a separate
# opt-in target, not part of `install`.
#
# Installs into the same GOBIN/$GOPATH/bin as `make install`, so it
# lands next to `clank`, where the preview flow's FindClankVoice looks
# for it first (then $PATH). Needs a C compiler (Xcode CLT / build-
# essential); CGO is forced on in case the environment disabled it. The
# ~670 MB Parakeet model set is fetched separately, on first use, by
# `clank preview`.
.PHONY: voice
voice:
	CGO_ENABLED=1 go -C voice-engine install ./cmd/clank-voice

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
# pkg/provisioner/flysprites/embed.go expects via //go:embed. The
# Sprites provisioner pushes this binary into a sprite via the SDK's
# filesystem API and registers it as a service.
#
# Pure-Go (CGO=0) so the cross-compile works on any host without
# needing a linux toolchain. -trimpath strips local filesystem paths
# from the binary for reproducibility.

.PHONY: embed-host
embed-host:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
	    -trimpath -buildvcs=false -o $(EMBED_HOST_BIN) \
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
# Brings up minio (image uploads) + clankd in containers. See
# docker/README.md for the full smoke recipe.

.PHONY: dev dev-rebuild
# One-command local dev: bring the docker stack (gateway + minio +
# auth-stub) up in the foreground; ctrl-c tears it down. Presigned S3 URLs
# resolve to the internal minio (clank-minio:9000) — no tunnel. See
# scripts/dev.sh for details.
dev:
	@bash scripts/dev.sh

# Like `make dev` but rebuilds the docker image from scratch first.
# Use when a code change isn't picking up — podman/docker layer cache
# can occasionally hold a stale `go build` layer when the surrounding
# context didn't visibly change.
dev-rebuild:
	docker compose -f docker/docker-compose.yml build --no-cache
	@bash scripts/dev.sh

.PHONY: docker-setup docker-prefs docker-up docker-down docker-build docker-logs
docker-setup:
	@if ! grep -q '^[^#]*[[:space:]]clank-minio\b' /etc/hosts; then \
	    echo "Adding 'clank-minio' → 127.0.0.1 to /etc/hosts (sudo prompt)..."; \
	    echo "127.0.0.1 clank-minio" | sudo tee -a /etc/hosts > /dev/null; \
	else \
	    echo "/etc/hosts already maps clank-minio — no change."; \
	fi
docker-prefs:
	@if [ ! -f docker/preferences.json ]; then \
	    echo "docker/preferences.json missing — copying from preferences.example.json."; \
	    echo "Edit it to set your provider + credentials; changes stay local (gitignored)."; \
	    cp docker/preferences.example.json docker/preferences.json; \
	fi
docker-build:
	docker compose -f docker/docker-compose.yml build
docker-up: docker-setup docker-prefs
	docker compose --env-file docker/.env -f docker/docker-compose.yml up -d --build 2>/dev/null \
	  || docker compose -f docker/docker-compose.yml up -d --build
docker-down:
	docker compose -f docker/docker-compose.yml down
docker-reset:
	docker compose -f docker/docker-compose.yml down -v
docker-logs:
	docker compose -f docker/docker-compose.yml logs -f --tail=100
