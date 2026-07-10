BINARY := alto
CMD := ./cmd/alto
FRONTEND_DIR := web/frontend

# Release version baked into the binary. Defaults to `git describe` (leading "v"
# stripped) so local builds carry a real version; falls back to the dev sentinel.
# Override with `make build ALTO_VERSION=2.4.1`.
ALTO_VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null))
VERSION_PKG := github.com/semsemyonoff/ALTO/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(if $(ALTO_VERSION),$(ALTO_VERSION),0.0.0)

# Docker image name and tag
ALTO_IMAGE ?= semsemyonoff/alto
ALTO_TAG ?= latest
# Target platforms for multi-arch build
ALTO_PLATFORMS ?= linux/amd64,linux/arm64

export ALTO_IMAGE ALTO_TAG ALTO_PLATFORMS

.PHONY: build test lint run docker-build image-build frontend-build dev

frontend-build:
	cd $(FRONTEND_DIR) && npm ci && npm run build

build: frontend-build
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test:
	go test ./...

lint:
	golangci-lint run

run:
	go run $(CMD)

# Run the Vite dev server and the Go server together for local frontend development.
dev:
	cd $(FRONTEND_DIR) && npm run dev & \
	ALTO_VITE_DEV=1 go run $(CMD); \
	kill %1

docker-build:
	docker build -t alto:latest .

# Build multi-arch image and push to registry
image-build:
	./build.sh
