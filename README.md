# ALTO — Audio Library Transcode Organizer

![Logo](static/logo.svg)

ALTO is a self-hosted web service for browsing and transcoding audio libraries. It provides a directory-tree UI for navigating mounted music collections, indexing audio metadata via ffprobe, and transcoding to FLAC or Opus via ffmpeg with real-time progress streaming.

![ALTO web dashboard](assets/alto-web.png)

## Features

- Directory-tree browser with lazy HTMX loading
- Audio metadata indexing: codec, bitrate, duration, sample rate, channels
- Cover art display (external files and embedded art extraction)
- Transcoding to FLAC (lossless) or Opus (lossy) with preset and custom options
- Three output modes: shared /out, local .alto-out/, or in-place replace with rollback
- Real-time SSE progress for both transcoding and re-indexing
- SQLite-backed index (WAL mode, concurrent reads)
- Docker-first deployment

## Quick Start

Use the published container image in your own `docker-compose.yml`:

```yaml
services:
  alto:
    image: semsemyonoff/alto:latest
    ports:
      - "8080:8080"
    environment:
      ALTO_LIBRARIES: "Music:/music,Lossless:/lossless"
      ALTO_PORT: "8080"
      ALTO_OUTPUT_DIR: "/out"
      ALTO_DB_PATH: "/data/alto.db"
      ALTO_CACHE_DIR: "/data/cache"
    volumes:
      - /path/to/your/music:/music:ro
      - /path/to/your/lossless:/lossless:ro
      - /path/to/output:/out
      - alto_data:/data
    restart: unless-stopped

volumes:
  alto_data:
```

Start it with:

```sh
docker compose up -d
```

Then open http://localhost:8080 in your browser.

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `ALTO_LIBRARIES` | (required) | Comma-separated `name:path` pairs, e.g. `Music:/music,Lossless:/lossless`. Names must match `[a-zA-Z0-9_-]`. |
| `ALTO_PORT` | `8080` | HTTP server port |
| `ALTO_OUTPUT_DIR` | `/out` | Shared output directory for transcoded files (Shared /out mode) |
| `ALTO_DB_PATH` | `./alto.db` | SQLite database file path |
| `ALTO_CACHE_DIR` | `./cache` | App-managed cache for extracted cover art — keep separate from library mounts |

## Docker Usage

### docker-compose.yml

```yaml
services:
  alto:
    image: semsemyonoff/alto:latest
    ports:
      - "8080:8080"
    environment:
      ALTO_LIBRARIES: "Music:/music,Lossless:/lossless"
      ALTO_PORT: "8080"
      ALTO_OUTPUT_DIR: "/out"
      ALTO_DB_PATH: "/data/alto.db"
      ALTO_CACHE_DIR: "/data/cache"
    volumes:
      - /path/to/your/music:/music:ro
      - /path/to/your/lossless:/lossless:ro
      - /path/to/output:/out
      - alto_data:/data
    restart: unless-stopped

volumes:
  alto_data:
```

**Read-only mounts** (`:ro`) are only safe when using **Shared `/out`** output mode.
**Local `.alto-out/`** and **Replace** modes write into the source directory and require a writable library mount (`:rw` or no flag).

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

## Local Development

```sh
# Build binary
make build

# Run locally (requires ALTO_LIBRARIES set)
ALTO_LIBRARIES="Music:/path/to/music" make run

# Run tests
make test

# Lint
make lint

# Build a local Docker image from the working tree
make docker-build
```

Local development requires Go `1.26.2`, `ffmpeg`, and `ffprobe` on `PATH`.

The checked-in [`docker-compose.yml`](docker-compose.yml) is development-oriented: it builds from the local working tree and expects you to replace the host bind mounts with paths available on your machine.

For multi-arch image publishing during development, use:

```sh
make image-build
```

This uses `build.sh` and pushes `${ALTO_IMAGE:-semsemyonoff/alto}:${ALTO_TAG:-latest}` for `${ALTO_PLATFORMS:-linux/amd64,linux/arm64}`.
