# Stage 1: Frontend build
FROM node:22-alpine AS frontend

WORKDIR /build/web/frontend

COPY web/frontend/package.json web/frontend/package-lock.json* ./
RUN npm ci

COPY web/frontend .
RUN npm run build

# Stage 2: Build
FROM golang:1.26-alpine AS builder

WORKDIR /build

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
COPY --from=frontend /build/web/static/dist /build/web/static/dist

# Release version, injected at build time from the release tag by build.sh
# (--build-arg APP_VERSION=…). Defaults to the dev sentinel for a plain
# `docker build`. Baked into the binary via -ldflags; the binary also honors an
# APP_VERSION env var at runtime, mirroring the beetDeck backend.
ARG APP_VERSION=0.0.0
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-X github.com/semsemyonoff/ALTO/internal/version.Version=${APP_VERSION}" \
    -o /alto ./cmd/alto

# Stage 3: Runtime
FROM alpine:3.24

# ffmpeg is the encoder, so it is pinned exactly rather than left to whatever
# the Alpine branch happens to carry on build day — two builds of the same ALTO
# tag must not produce different audio. Alpine keeps only the current version
# per branch, so when the package rotates this `apk add` fails loudly instead of
# quietly upgrading; bump FFMPEG_VERSION (and smoke-test a real FLAC and Opus
# job) as a deliberate change. Override ad hoc with
# `--build-arg FFMPEG_VERSION=…`.
ARG FFMPEG_VERSION=8.1.2-r0
RUN apk add --no-cache "ffmpeg=${FFMPEG_VERSION}" \
    && ffmpeg -version | head -1

WORKDIR /app
COPY --from=builder /alto /app/alto
COPY --from=builder /build/web /app/web

EXPOSE 8080

ENTRYPOINT ["/app/alto"]
