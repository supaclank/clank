# ---- Build / install -------------------------------------------------

# Path of the cross-compiled clank-host binary embedded into the
# Sprites provisioner. A gitignored build artifact rebuilt by
# `make embed-host`, which install/test depend on. Path matches the
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
	CGO_ENABLED=1 GOWORK=off go -C voice-engine install ./cmd/clank-voice

.PHONY: test test-race
test: embed-host
	go test ./...

test-race: embed-host
	go test -race ./...

# ---- Code generation -------------------------------------------------
#
# Regenerates sqlc-derived code from each store's declarative schema.sql
# and queries/. Run after editing either, then commit the generated
# files alongside.
#
# Requires sqlc on PATH (`brew install sqlc` on macOS).

# Every SQLite store package: holds schema.sql (declarative truth,
# read by sqlc), migrations/ (goose files applied at Open()), and
# sqlc.yaml. New stores join this list to inherit the migration
# tooling below.
SQLITE_STORES := internal/store internal/host/store

.PHONY: generate
generate:
	@for s in $(SQLITE_STORES); do sqlc generate -f $$s/sqlc.yaml || exit 1; done

# ---- Schema migrations -----------------------------------------------
#
# Declarative workflow: edit <store>/schema.sql to the desired shape,
# then have Atlas generate the goose migration implementing the change:
#
#   make migration store=internal/store name=add_some_column
#
# Review the generated file in <store>/migrations/, then `make generate`
# to refresh sqlc code, and commit all three together. goose applies
# migrations/ at Open(); Atlas is only ever a dev/CI tool.
#
# Requires atlas on PATH (`brew install atlas` on macOS).

.PHONY: migration
migration:
	@test -n "$(store)" && test -n "$(name)" || \
	    { echo "usage: make migration store=internal/store name=add_some_column"; exit 1; }
	atlas migrate diff $(name) \
	    --dir "file://$(store)/migrations?format=goose" \
	    --to "file://$(store)/schema.sql" \
	    --dev-url "sqlite://dev?mode=memory"

# Lints migrations added since LINT_BASE (default origin/main) for
# destructive/risky changes; Atlas exits non-zero on findings (e.g.
# DS103 column drop), failing CI. https://atlasgo.io/lint/analyzers
.PHONY: migrations-lint
migrations-lint:
	@for s in $(SQLITE_STORES); do \
	    atlas migrate lint \
	        --dir "file://$$s/migrations?format=goose" \
	        --dev-url "sqlite://dev?mode=memory" \
	        --git-base "$(or $(LINT_BASE),origin/main)" || exit 1; \
	done

# Verifies migrations/ ≡ schema.sql for every store (CI runs this). A
# drifted schema makes the diff emit a new migration file, which the
# git-clean check turns into a failure.
.PHONY: migrations-check
migrations-check:
	@for s in $(SQLITE_STORES); do \
	    atlas migrate diff ci_drift_check \
	        --dir "file://$$s/migrations?format=goose" \
	        --to "file://$$s/schema.sql" \
	        --dev-url "sqlite://dev?mode=memory" || exit 1; \
	done
	@drift="$$(git status --porcelain -- $(addsuffix /migrations,$(SQLITE_STORES)))"; \
	test -z "$$drift" || { \
	    echo "$$drift"; \
	    echo "schema.sql and migrations/ have drifted — inspect the generated ci_drift_check file,"; \
	    echo "regenerate it with a real name via 'make migration', and commit it."; \
	    exit 1; \
	}

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
