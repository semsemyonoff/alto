#!/usr/bin/env bash
# Build and push the multi-arch ALTO production image from this repository.
# Build context is the repo root (see Dockerfile: the frontend is built in-image,
# so no pre-build step is needed).
#
# Shared by the local `make release` AND by both CI pipelines (GitHub Actions →
# Docker Hub + GHCR; Forgejo Actions → git.horn/alto/alto). The caller picks the
# targets:
#
#   ALTO_IMAGES    space/newline-separated list of image refs WITHOUT tag
#                  (default: semsemyonoff/alto). Every image is tagged with every
#                  ALTO_TAGS value in a single buildx --push, so the image is
#                  built ONCE and fanned out to all targets.
#   ALTO_TAGS      space-separated list of tags (default: latest)
#   ALTO_VERSION   product version baked into the image as APP_VERSION (reported
#                  by the app). Defaults to the first non-"latest" tag, then to
#                  ./VERSION, then to `git describe`, then 0.0.0 — so CI needs no
#                  extra wiring (the release version is already the first tag).
#   ALTO_PLATFORMS buildx platforms (default: linux/amd64,linux/arm64)
#   ALTO_BUILDER   buildx builder to use. Set to "default" on a daemon with the
#                  containerd image store (multi-arch-builds AND pushes through
#                  the daemon itself, inheriting its DNS + registry CA trust —
#                  needed for the internal git.horn push from the Forgejo dind
#                  runner). Unset → manage a docker-container builder (GitHub
#                  runners, local Docker without the containerd store).
#
# `docker login` to each target registry must already be done by the caller.
# Back-compat: the old singular ALTO_IMAGE / ALTO_TAG are still honored.
set -euo pipefail
cd "$(dirname "$0")"

IMAGES="${ALTO_IMAGES:-${ALTO_IMAGE:-semsemyonoff/alto}}"
TAGS="${ALTO_TAGS:-${ALTO_TAG:-latest}}"
PLATFORMS="${ALTO_PLATFORMS:-linux/amd64,linux/arm64}"

# Product version baked into the image (APP_VERSION). Prefer an explicit
# ALTO_VERSION; otherwise take the first tag that isn't "latest" (CI passes
# "$version latest"), then fall back to ./VERSION, then `git describe`, then
# 0.0.0. Stored bare (leading "v" stripped).
VERSION="${ALTO_VERSION:-}"
if [ -z "$VERSION" ]; then
    for t in $TAGS; do
        if [ "$t" != latest ]; then VERSION="$t"; break; fi
    done
fi
if [ -z "$VERSION" ]; then
    VERSION="$(cat VERSION 2>/dev/null || true)"
fi
if [ -z "$VERSION" ]; then
    VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi
VERSION="${VERSION:-0.0.0}"
VERSION="${VERSION#v}"

# Fan out: one --tag per (image, tag) pair → built once, pushed everywhere.
tag_args=()
refs=()
for img in $IMAGES; do
    for t in $TAGS; do
        tag_args+=( --tag "${img}:${t}" )
        refs+=( "${img}:${t}" )
    done
done
echo ">> building ${PLATFORMS} (APP_VERSION=${VERSION}) and pushing:"
printf '   %s\n' "${refs[@]}"

# Pick the buildx builder (see ALTO_BUILDER above).
if [ -n "${ALTO_BUILDER:-}" ]; then
    docker buildx use "$ALTO_BUILDER"
else
    BUILDER="alto-multiarch"
    if ! docker buildx inspect "$BUILDER" &>/dev/null; then
        docker buildx create --name "$BUILDER" --use
    else
        docker buildx use "$BUILDER"
    fi
fi

docker buildx build \
    --platform "$PLATFORMS" \
    --build-arg "APP_VERSION=${VERSION}" \
    "${tag_args[@]}" \
    --push .
