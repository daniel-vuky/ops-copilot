# Ops Copilot developer entry point (ADR 0001 §14).
#
# Every Go and Node command runs inside the pinned toolchain image, so the host
# needs only Docker, make and git. Recipes and the docker wrapper use make's
# default /bin/sh (POSIX only, no bash features). Works with GNU make 3.81
# (macOS default). Run make from the main checkout, not a git worktree.

.DEFAULT_GOAL := help

# The toolchain image is tagged by the git hash of its Dockerfile, so any edit
# to the Dockerfile gives a new tag and the next target rebuilds the image.
# Only the Dockerfile is hashed: if it ever COPYs a file from build/toolchain,
# fold that file into the hash too, or edits to it will not trigger a rebuild.
TOOLCHAIN_DOCKERFILE := build/toolchain/Dockerfile
TOOLCHAIN_TAG := $(shell git hash-object $(TOOLCHAIN_DOCKERFILE) 2>/dev/null | cut -c1-12)
ifeq ($(TOOLCHAIN_TAG),)
$(error cannot hash build/toolchain/Dockerfile; is git installed?)
endif
TOOLCHAIN_IMAGE := ops-copilot-toolchain:$(TOOLCHAIN_TAG)

# Runs a command in the toolchain as the host user over the bind-mounted
# checkout, with the shared cache volume at /cache. CGO_ENABLED=0 is passed
# here too (ADR 0001 §3) so build, test and lint get it even if the image
# env changes.
# - --init: a real PID 1 forwards SIGINT/SIGTERM (node, pnpm and bash as
#   PID 1 ignore them, so Ctrl-C would not stop the container).
# - TTY: -it is added at recipe time, only when both stdin and stdout are
#   terminals (CI, pipes and redirects get no TTY). The test runs in the body
#   of a small sh wrapper, where fd 0 and fd 1 are the recipe's own (after
#   any redirection on the recipe line), not inside $$(...), where fd 1 would
#   be the capture pipe. The `$$` survives `:=` as a single `$`, so nothing
#   is decided at parse time.
RUN_ARGS := --rm --init --user $(shell id -u):$(shell id -g) --cap-drop ALL --security-opt no-new-privileges -e CGO_ENABLED=0 -v "$(CURDIR)":/src -w /src -v ops-copilot-cache:/cache $(TOOLCHAIN_IMAGE)
TTY_WRAP = sh -c 'if test -t 0 && test -t 1; then set -- -it "$$@"; fi; $(1) "$$@"' ops-copilot-run
RUN := $(call TTY_WRAP,exec docker run) $(RUN_ARGS)

.PHONY: help toolchain .toolchain-ready shell check lint test build web-deps lint-web build-web lint-go test-go build-go clean print-image print-run

help: ## Show this help
	@echo 'Usage: make <target>'
	@echo
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z][a-zA-Z0-9_-]*:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

toolchain: ## Build the pinned toolchain image
	docker build -t $(TOOLCHAIN_IMAGE) build/toolchain

# Internal: build the toolchain image only when this tag is missing. Every
# in-container target depends on it.
.toolchain-ready:
	@docker image inspect $(TOOLCHAIN_IMAGE) >/dev/null 2>&1 || $(MAKE) toolchain

shell: .toolchain-ready ## Open an interactive bash shell in the toolchain
	$(RUN) bash

# Aggregate targets. Order is a recipe of sequential recursive makes, not
# prerequisites, so make -j cannot reorder or interleave the steps: the
# frontend goes before Go because the binary will embed web/dist (SU-07).
check: ## Run lint, test and build, stopping at the first failure
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) build

lint: ## Lint the frontend, then the Go code
	$(MAKE) lint-web
	$(MAKE) lint-go

# Frontend tests arrive with SU-07; until then test runs the Go tests only.
test: ## Run the tests
	$(MAKE) test-go

build: ## Build the frontend into web/dist, then the Go binary
	$(MAKE) build-web
	$(MAKE) build-go

web-deps: .toolchain-ready ## Install the frontend dependencies from the lockfile
	$(RUN) pnpm -C web install --frozen-lockfile

# Scripts are always called with `run`: in pnpm 12 a built-in command wins
# over a script of the same name.
lint-web: web-deps ## Lint and type-check the frontend (ESLint, tsc)
	$(RUN) pnpm -C web run lint

build-web: web-deps ## Build the frontend into web/dist with Vite
	$(RUN) pnpm -C web run build

# Go's ./... skips web/node_modules only because pnpm's isolated linker puts
# the real packages under .pnpm (a dot-dir, which Go ignores) and the
# top-level entries are symlinks (which Go does not follow). Switching to
# nodeLinker: hoisted or shamefullyHoist would pull .go files from npm
# packages into ./... . Do not add a web/go.mod to fence it off either: it
# would break the go:embed of web/dist planned for SU-07.
lint-go: .toolchain-ready ## Lint the Go code with golangci-lint
	$(RUN) golangci-lint run ./...

test-go: .toolchain-ready ## Run the Go tests
	$(RUN) go test ./...

# CGO_ENABLED=0 comes from RUN and the image env (ADR 0001 §3).
build-go: .toolchain-ready ## Build the static binary into bin/ops-copilot
	$(RUN) go build -trimpath -ldflags "-s -w" -o bin/ops-copilot ./cmd/ops-copilot

clean: ## Remove build outputs (bin, web/dist)
	rm -rf bin web/dist

# Hidden helpers for scripts and checks.
print-image:
	@echo $(TOOLCHAIN_IMAGE)

# Prints the same docker command RUN executes, as a plain prefix for
# $$(make -s print-run) <cmd>: the TTY test is evaluated (captured that way,
# stdout is a pipe, so it never carries -it) and the quotes around the
# checkout path are stripped by the shell.
print-run:
	@$(call TTY_WRAP,echo docker run) $(RUN_ARGS)
