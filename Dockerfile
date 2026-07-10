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
FROM alpine:3.21

RUN apk add --no-cache ffmpeg

WORKDIR /app
COPY --from=builder /alto /app/alto
COPY --from=builder /build/web /app/web

EXPOSE 8080

ENTRYPOINT ["/app/alto"]
