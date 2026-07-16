# ALTO — Audio Library Transcode Organizer

<img src="static/logo.svg" alt="ALTO logo" width="200" height="200">

ALTO is a self-hosted web UI for **converting your music library to another audio format** — without touching the command line.

Point it at the folders where your music lives, open it in a browser, click through to an album, pick a format, and press Transcode. ffmpeg does the work on the server; you watch the progress bar. Nothing is uploaded anywhere and nothing leaves your machine.

<img src="assets/alto.png" alt="ALTO web dashboard" width="820" style="max-width:100%;">

## What it's for

Typical reasons people run ALTO:

- **Shrink a lossless collection for a phone or a car.** A FLAC library converted to Opus at 160k is roughly a fifth of the size and still transparent to the ear. Keep the originals, put the Opus copy on the device.
- **Normalize a messy library to one format.** Downloads arrive as a mix of WAV, ALAC, high-bitrate MP3. Convert a directory tree to a single consistent format in one pass.
- **Re-encode in place, safely.** Replace mode rewrites files where they sit — atomically, one file at a time, with an automatic rollback if ffmpeg fails, so a bad run can't shred your library.
- **See what's actually in your library.** ALTO probes every file and shows codec, bitrate, sample rate, channels, and duration — so you can find the 320k MP3s hiding among the FLACs before deciding what to convert.

## Features

### Browsing your library

- Directory-tree browser over any number of mounted libraries, with a library switcher showing per-library track counts and index status.
- Every audio file is probed with `ffprobe` and indexed into SQLite: codec, bitrate, duration, sample rate, channels — visible per track without opening a player.
- Cover art is shown for each directory, both from external files (`cover.jpg` and friends) and extracted from embedded tags.
- Server-side search jumps straight to a directory by name instead of clicking down the tree.

### Transcoding

- **Two output formats**: FLAC (lossless, for archiving) and Opus (lossy, for devices).
- **Presets** for both, so you don't need to know ffmpeg flags — Fast / Balanced / Max Compression for FLAC, 128k / 160k / 192k for Opus. See [Transcoding Presets](#transcoding-presets).
- **Custom mode** for when you do: set bitrate or compression level directly, toggle metadata and cover-art copying, or pass raw ffmpeg arguments.
- **Three output modes** — write to a shared output directory, drop an `.alto-out/` folder next to the source, or replace the originals in place with rollback. See [Output Modes](#output-modes).
- **A job queue** with a configurable number of parallel workers (`ALTO_TRANSCODE_WORKERS`). Queue a dozen albums and walk away; jobs beyond capacity wait their turn, and the queue panel — visible on every screen — lets you watch or cancel any of them.
- **Live progress**, streamed over SSE for both transcoding and library re-indexing. No page refreshing to find out where it's at.

### Deployment

- One Docker image with ffmpeg baked in — no separate ffmpeg install, no build step.
- SQLite index in WAL mode; no external database to run alongside it.
- Works on a phone or tablet as well as a desktop: the tree collapses into a drawer, the transcode panel becomes a slide-in sheet, and a floating Transcode button opens it.

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
