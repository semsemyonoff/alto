# Changelog

All notable changes to ALTO are documented here. ALTO ships as a single product
version — each entry corresponds to one published `semsemyonoff/alto` image tag
built from this repository (Go backend + web frontend live together in one repo).

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

<!-- Write notes for the next release here. "Cut release" promotes this
     section to ## [X.Y.Z] - <date> and uses it as the release body. -->

## [0.1.0] - 2026-07-11

Initial release of **ALTO** — a self-hosted web service for browsing and
transcoding audio libraries.

Ships as a single multi-arch image (linux/amd64 + linux/arm64), published to the
internal `git.horn/alto/alto` registry and the public `semsemyonoff/alto` (Docker
Hub) and `ghcr.io/semsemyonoff/alto` (GHCR) registries.

### Added
- Directory-tree browser with lazy HTMX loading and server-side tree search.
- Audio metadata indexing via ffprobe (codec, bitrate, duration, sample rate,
  channels) with cover-art display (external files and embedded art extraction).
- Transcoding to FLAC (lossless) or Opus (lossy) with presets and custom ffmpeg
  parameters, plus three output modes: shared `/out`, local `.alto-out/`, and
  in-place replace with rollback.
- Bounded transcode worker pool (`ALTO_TRANSCODE_WORKERS`) with a global queue
  panel: jobs beyond capacity wait as `queued`, run in order, and can be canceled.
- Multi-library selector with per-library indexed status and track counts.
- Real-time SSE progress for transcoding and re-indexing.
- Responsive layout for tablet and mobile (drawer tree, slide-in transcode dock,
  floating Transcode button).
- SQLite-backed index (WAL mode, concurrent reads).
- Deployment layer: production `docker-compose.yml`, `.env.example`, operator
  `Makefile` targets, and a self-contained multi-stage `Dockerfile` (frontend
  build + Go build + runtime) that builds the release image entirely from this
  repository.
- Release CI: the "Cut release" button tags `vX.Y.Z`, which fans out to the build
  pipelines — Forgejo Actions publishes to `git.horn/alto/alto`; GitHub Actions
  (via the push mirror) publishes to Docker Hub and GHCR.
- Version readout baked into the image at build time (`APP_VERSION`), surfaced in
  the UI and reported by the app.
