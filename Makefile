# ---- Build / install -------------------------------------------------

.PHONY: install
install:
	go install ./cmd/clank/ ./cmd/clankd/ ./cmd/clank-host/

.PHONY: test test-race
test:
	go test ./...

test-race:
	go test -race ./...

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

# ---- Local dev: cloud-hub-on-laptop with auto-tunnel -----------------
#
# Spawns a Cloudflare quick tunnel and starts `clankd` with the tunnel
# URL plumbed in as --public-base-url, so a Daytona (or any cloud)
# sandbox can reach back into the laptop without a VPN. The tunnel
# rotates on every invocation. Ctrl-C stops both.
#
# Override behavior with env vars consumed by scripts/dev-cloud-hub.sh:
#   CLANK_DIR  data dir for the cloud hub. Default: ~/.clank-cloud
#   LISTEN     listener address. Default: tcp://0.0.0.0:7878
#
# Production cloud hubs don't need this — they sit on a real domain.

.PHONY: cloud-hub
cloud-hub:
	@bash scripts/dev-cloud-hub.sh
