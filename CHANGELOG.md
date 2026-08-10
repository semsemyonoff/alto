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
- `ALTO_SCAN_WORKERS` (default `0` = `min(4, NumCPU)`) — caps concurrent
  `ffprobe` processes across all libraries during a scan.

### Changed
- Library scanning is now incremental: each track stores its file size and
  mtime, and a file whose `(size, mtime)` still matches the indexed row is
  reused without spawning `ffprobe`. A re-scan of an unchanged library is a
  walk plus `stat` pass. The first scan after upgrading is still a full one —
  the schema migrates in place, and the backfilled `mtime = 0` never matches a
  real file.
- Embedded cover art is read from the track that was just probed instead of
  triggering a second `ffprobe` of the directory's first audio file, so each
  file is probed at most once per scan.
- A directory's tracks are written in a single transaction instead of one per
  track, and the write is skipped entirely when nothing in the directory
  changed.

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

