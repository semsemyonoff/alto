#!/usr/bin/env bash
set -euo pipefail

IMAGE="${ALTO_IMAGE:-semsemyonoff/alto}"
TAG="${ALTO_TAG:-latest}"
PLATFORMS="${ALTO_PLATFORMS:-linux/amd64,linux/arm64}"

BUILDER="alto-multiarch"
if ! docker buildx inspect "$BUILDER" &>/dev/null; then
    docker buildx create --name "$BUILDER" --use
else
    docker buildx use "$BUILDER"
fi

docker buildx build \
    --platform "$PLATFORMS" \
    --tag "${IMAGE}:${TAG}" \
    --push .
