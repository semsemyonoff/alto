.PHONY: build test lint run docker-build

BINARY := alto
CMD := ./cmd/alto

# Docker image name and tag
ALTO_IMAGE ?= semsemyonoff/alto
ALTO_TAG ?= latest
# Target platforms for multi-arch build
ALTO_PLATFORMS ?= linux/amd64,linux/arm64

export ALTO_IMAGE ALTO_TAG ALTO_PLATFORMS

.PHONY: build test lint run docker-build image-build

build:
	go build -o $(BINARY) $(CMD)

test:
	go test ./...

lint:
	golangci-lint run

run:
	go run $(CMD)

docker-build:
	docker build -t alto:latest .

# Build multi-arch image and push to registry
image-build:
	./build.sh
