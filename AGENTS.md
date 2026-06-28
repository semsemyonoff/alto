# Repository Guidelines

## Project Structure & Module Organization
`cmd/alto` contains the main entrypoint, config parsing, and startup wiring. Core application code lives under `internal/`: `db` for SQLite access, `library` for scanning and `ffprobe` metadata, `server` for HTTP handlers and HTMX/SSE flows, and `transcode` for ffmpeg job execution. Runtime templates and frontend assets live in `web/templates` and `web/static`. Top-level `assets/` and `static/` are documentation and branding assets, not application runtime code. Keep long-form planning notes in `docs/plans`; repository automation state under `.ralphex/` is not product code.

## Build, Test, and Development Commands
Use the `Makefile` targets first:

- `make build` builds the `alto` binary from `./cmd/alto`.
- `make test` runs `go test ./...` across all packages.
- `make lint` runs `golangci-lint run`.
- `ALTO_LIBRARIES="Music:/path/to/music" make run` starts the app locally.
- `make docker-build` builds a local Docker image as `alto:latest`.
- `make image-build` builds and pushes a multi-arch image using `build.sh`; defaults are `semsemyonoff/alto:latest` and `linux/amd64,linux/arm64`.
- `docker compose up -d` runs the checked-in development stack after you update the bind mounts in `docker-compose.yml` for your machine.

Local development expects Go `1.26.2`, plus `ffmpeg` and `ffprobe` on `PATH`. Docker is required only for container workflows.

## Coding Style & Naming Conventions
Follow standard Go formatting with `gofmt`; do not hand-format Go files to match the global EditorConfig spacing. Use lower-case package names, `CamelCase` for exported identifiers, and descriptive handler/test names such as `TestHandleTree_InvalidID`. For non-Go files, `.editorconfig` sets 4-space indentation by default, 2 spaces for `yml`, `yaml`, `json`, and `sh`, and tabs for `Makefile`.

## Testing Guidelines
Keep tests next to the code they cover in `*_test.go` files. Prefer table-driven tests and `t.TempDir()` or in-memory SQLite for isolated filesystem and DB coverage. When changing env parsing or startup behavior, extend tests under `cmd/alto`; when changing HTTP or UI behavior, extend the server tests in `internal/server`; when changing scanning or transcoding logic, add cases under `internal/library` or `internal/transcode`. Run `make test` and `make lint` before opening a PR.

## Commit & Pull Request Guidelines
Recent history uses Conventional Commit prefixes, mainly `feat:` and `fix:`, with imperative summaries like `feat: transcoding API with job management`. Keep commits focused and similarly formatted. PRs should explain behavior changes, note any environment or mount-mode implications, link related issues, and include screenshots for template/CSS/UI changes. List the verification commands you ran in the PR description.

## Configuration Tips
`ALTO_LIBRARIES` is required and uses `name:path` pairs such as `Music:/music`. Library names must match `[a-zA-Z0-9_-]`, and library names and paths must be unique. `ALTO_OUTPUT_DIR` defaults to `/out`; if you point it inside a library root, the app will warn and exclude that directory from scans. `ALTO_CACHE_DIR` should stay outside library mounts. Read-only mounts are safe only for shared `/out` mode; local `.alto-out/` and replace mode require writable library mounts.
