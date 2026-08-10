# Changelog

All notable changes to ALTO are documented here.

The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

<!-- Write notes for the next release here. "Cut release" promotes this
     section to ## [X.Y.Z] - <date> and uses it as the release body. -->

**Breaking:** JSON endpoints now return `{"error", "code", …}` instead of
`text/plain`. Match on `code`, not on the message.

### Added
- `GET /api/openapi.yaml` — an OpenAPI 3.1 document covering every `/api/*`
  route, schema and error code. Tests keep it in sync with the code.
- **Per-track transcoding of mixed directories.** Tick *Skip lossy* or pick
  tracks in the new checkbox column instead of being refused the whole album.
  `POST /api/transcode` gained `skip_lossy`, `files` and `copy_skipped`; its
  `202` now names what was scheduled and what was skipped.
- New endpoints: `GET /api/presets`, `GET /api/jobs/{id}`, `GET /api/scan/state`,
  `POST /api/scan/dir?path=`.
- `422 output_name_conflict` — two sources producing the same output name are
  refused instead of one silently overwriting the other.
- `ALTO_SCAN_ON_START` (default `true`) skips the startup re-index when `false`;
  `ALTO_SCAN_WORKERS` (default `min(4, NumCPU)`) caps concurrent `ffprobe`.

### Changed
- **Scanning is incremental.** A file whose `(size, mtime)` matches the index is
  reused without `ffprobe`, so a re-scan of an unchanged library is a walk plus
  `stat`. The schema migrates in place; the first scan after upgrading is still
  a full one.
- Finished jobs are tombstoned, not deleted — `GET /api/jobs/{id}` still answers
  after the job leaves the queue, and `404 job_not_found` now means the id never
  existed.

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

