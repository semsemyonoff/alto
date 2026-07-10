#!/usr/bin/env bash
set -euo pipefail

IMAGE="${ALTO_IMAGE:-semsemyonoff/alto}"
TAG="${ALTO_TAG:-latest}"
PLATFORMS="${ALTO_PLATFORMS:-linux/amd64,linux/arm64}"

# Release version baked into the image (--build-arg APP_VERSION). Precedence:
# explicit $ALTO_VERSION → the image tag (when it isn't "latest") → git describe
# → the dev sentinel. Stored bare (leading "v" stripped), matching beetDeck.
VERSION="${ALTO_VERSION:-}"
if [ -z "$VERSION" ] && [ "$TAG" != "latest" ]; then
    VERSION="$TAG"
fi
if [ -z "$VERSION" ]; then
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi
VERSION="${VERSION:-0.0.0}"
VERSION="${VERSION#v}"

BUILDER="alto-multiarch"
if ! docker buildx inspect "$BUILDER" &>/dev/null; then
    docker buildx create --name "$BUILDER" --use
else
    docker buildx use "$BUILDER"
fi

docker buildx build \
    --platform "$PLATFORMS" \
    --build-arg "APP_VERSION=${VERSION}" \
    --tag "${IMAGE}:${TAG}" \
    --push .
