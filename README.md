# ALTO — Audio Library Transcode Organizer

<img src="static/logo.svg" alt="ALTO logo" width="200" height="200">

ALTO is a self-hosted web service for browsing and transcoding audio libraries. It provides a directory-tree UI for navigating mounted music collections, indexing audio metadata via ffprobe, and transcoding to FLAC or Opus via ffmpeg with real-time progress streaming.

<img src="assets/alto.png" alt="ALTO web dashboard" width="820" style="max-width:100%;">

## Features

- Directory-tree browser with lazy HTMX loading
- Audio metadata indexing: codec, bitrate, duration, sample rate, channels
- Cover art display (external files and embedded art extraction)
- Transcoding to FLAC (lossless) or Opus (lossy) with preset and custom options
- Three output modes: shared /out, local .alto-out/, or in-place replace with rollback
- Transcode queue with a bounded worker pool (`ALTO_TRANSCODE_WORKERS`): jobs beyond capacity wait as `queued`, run in order, and can be canceled from a global queue panel present on every screen
- Multi-library selector showing per-library indexed status and track counts
- Server-side directory tree search
- Real-time SSE progress for both transcoding and re-indexing
- Responsive layout for tablet and mobile: the directory tree collapses into a hamburger drawer, the transcode dock becomes a slide-in panel (bottom sheet on phones), and a floating Transcode button opens it
- SQLite-backed index (WAL mode, concurrent reads)
- Docker-first deployment

## Quick Start

ALTO ships as a single Docker image. You need [Docker](https://docs.docker.com/get-docker/)
with Compose. The repository's [`docker-compose.yml`](docker-compose.yml) pulls a
published image and is driven entirely by a `.env` file:

```sh
cp .env.example .env       # set image/tag, host port, and your library paths
docker compose up -d
```

Then open **http://localhost:8080** (or whatever `ALTO_HTTP_PORT` you set).

`.env` maps your host directories into the container and lists them in
`ALTO_LIBRARIES`; see the [Configuration](#configuration) table below and the
comments in [`.env.example`](.env.example). Your SQLite index and cover-art cache
persist in the `alto_data` Docker volume, so they survive restarts and upgrades.

> **Read-only mounts** (`:ro`, the default in `docker-compose.yml`) are only safe
> with **Shared `/out`** output mode. The **Local `.alto-out/`** and **Replace**
> modes write into the source directory and need a writable library mount — drop
> the `:ro` for those.

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `ALTO_LIBRARIES` | (required) | Comma-separated `name:path` pairs, e.g. `Music:/music,Lossless:/lossless`. Names must match `[a-zA-Z0-9_-]`. |
| `ALTO_PORT` | `8080` | HTTP server port |
| `ALTO_OUTPUT_DIR` | `/out` | Shared output directory for transcoded files (Shared /out mode) |
| `ALTO_DB_PATH` | `./alto.db` | SQLite database file path |
| `ALTO_CACHE_DIR` | `./cache` | App-managed cache for extracted cover art — keep separate from library mounts |
| `ALTO_TRANSCODE_WORKERS` | `1` | Number of concurrent transcode jobs; additional jobs sit `queued` until a worker is free |

## Running It

The [`Makefile`](Makefile) wraps the common operator actions (all read `.env`):

```sh
make up       # start the stack
make down     # stop and remove it
make logs     # tail the logs
make ps       # container status
make pull     # pull a newer image tag
make restart  # recreate the stack
```

### Choosing an image

`ALTO_IMAGE` / `ALTO_TAG` in `.env` select what runs. Pin `ALTO_TAG` to a release
(e.g. `0.1.0`) in production; `latest` is fine for trying it out. Available images:

| Registry | Image |
|---|---|
| Docker Hub | `semsemyonoff/alto` |
| GHCR | `ghcr.io/semsemyonoff/alto` |

### Upgrading

Bump `ALTO_TAG` in `.env`, then:

```sh
make pull && make up
```

## Transcoding Presets

### FLAC

All FLAC presets: metadata copy on, cover art copy on, verify on.

| Preset | Compression Level | Notes |
|---|---|---|
| Fast | 0 | Fastest encode, largest file |
| Balanced (default) | 5 | Good balance of speed and size |
| Max Compression | 8 | Slowest, smallest file |

### Opus

All Opus presets: VBR on, application=audio, compression_level=10, source channel layout preserved.

| Preset | Bitrate | Notes |
|---|---|---|
| Music Balanced | 128k | Transparent for most content |
| Music High (default) | 160k | Recommended default |
| Archive Lossy | 192k | High-quality archive |

### Custom Parameters

Select "Custom" in the preset dropdown to configure manually:

- Bitrate (Opus) or compression level (FLAC)
- Metadata copy flag
- Cover art copy flag
- Additional raw ffmpeg arguments (advanced)

## Output Modes

| Mode | Description |
|---|---|
| Shared /out (default) | Mirrors library path structure under `<ALTO_OUTPUT_DIR>/<library-name>/<relative-path>/`. Non-audio files copied alongside. |
| Local out | Creates `.alto-out/` subdirectory inside the source audio directory. |
| Replace | Atomic per-file in-place replacement with rollback. Backup created on same filesystem; restored automatically on failure. Requires confirmation. |
