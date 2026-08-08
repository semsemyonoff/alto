BINARY := alto
CMD := ./cmd/alto
FRONTEND_DIR := web/frontend
SHELL := /bin/bash

# Release version baked into the binary. Defaults to `git describe` (leading "v"
# stripped) so local builds carry a real version; falls back to the dev sentinel.
# Override with `make build ALTO_VERSION=2.4.1`.
ALTO_VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null))
VERSION_PKG := github.com/semsemyonoff/ALTO/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(if $(ALTO_VERSION),$(ALTO_VERSION),0.0.0)

# Release image targets. CI passes its own ALTO_IMAGES (GitHub → Docker Hub +
# GHCR; Forgejo → git.horn/alto/alto). These are the LOCAL-fallback defaults.
IMAGES ?= semsemyonoff/alto
# Product version for `make release`, read from ./VERSION.
RELEASE_VERSION ?= $(shell cat VERSION 2>/dev/null)

# Docker image name and tag (dev image-build path).
ALTO_IMAGE ?= semsemyonoff/alto
ALTO_TAG ?= latest
# Target platforms for multi-arch build
ALTO_PLATFORMS ?= linux/amd64,linux/arm64

export ALTO_IMAGE ALTO_TAG ALTO_PLATFORMS

.PHONY: help build test test-race lint run docker-build image-build frontend-build dev \
        up down restart pull logs ps release release-local

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## ----- Development -----

frontend-build: ## Build the Vite frontend bundle
	cd $(FRONTEND_DIR) && npm ci && npm run build

build: frontend-build ## Build the alto binary (frontend included)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test: ## Run the Go test suite
	go test ./...

test-race: ## Run the Go test suite under the race detector (needs cgo)
	CGO_ENABLED=1 go test -race ./...

lint: ## Run golangci-lint
	golangci-lint run

run: ## Run the app locally (requires ALTO_LIBRARIES)
	go run $(CMD)

dev: ## Run the Vite dev server and the Go server together (frontend HMR)
	cd $(FRONTEND_DIR) && npm run dev & \
	ALTO_VITE_DEV=1 go run $(CMD); \
	kill %1

docker-build: ## Build a local single-arch image (alto:latest) from the working tree
	docker build -t alto:latest .

image-build: ## Build & push a multi-arch dev image via build.sh
	./build.sh

## ----- Operator targets (run a published image via docker-compose) -----

up: ## Start the stack (reads .env)
	docker compose up -d

down: ## Stop and remove the stack
	docker compose down

restart: ## Recreate the stack
	docker compose up -d --force-recreate

pull: ## Pull the image tag pinned in .env
	docker compose pull

logs: ## Tail logs
	docker compose logs -f

ps: ## Show container status
	docker compose ps

## ----- Maintainer targets (build & publish a release from this repo) -----

release: ## Build multi-arch image RELEASE_VERSION + latest and push. CI is the primary path; this is a local fallback.
	@test -n "$(RELEASE_VERSION)" || { echo "ERROR: set RELEASE_VERSION (or write it to ./VERSION)" >&2; exit 1; }
	ALTO_IMAGES="$(IMAGES)" ALTO_TAGS="$(RELEASE_VERSION) latest" ./build.sh

release-local: ## Build a single-arch image locally (no push) for testing
	@test -n "$(RELEASE_VERSION)" || { echo "ERROR: set RELEASE_VERSION" >&2; exit 1; }
	docker build --build-arg "APP_VERSION=$(RELEASE_VERSION)" -t "$(firstword $(IMAGES)):$(RELEASE_VERSION)" .
