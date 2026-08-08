# Changelog

All notable changes to ALTO are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

<!-- Write notes for the next release here. "Cut release" promotes this
     section to ## [X.Y.Z] - <date> and uses it as the release body. -->

### Added
- `ALTO_SCAN_ON_START` (default `true`) — set to `false` to skip the startup
  library re-index on large collections where a full scan on every restart is
  too costly. Manual re-index (UI button / `POST /api/scan`) is unaffected.

## [0.1.0] - 2026-07-11

Initial release of **ALTO** — a self-hosted web service for browsing and
transcoding audio libraries.

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
- Version readout surfaced in the UI.

