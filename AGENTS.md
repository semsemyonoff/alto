# Repository Guidelines

## Project Structure & Module Organization
`cmd/alto` contains the main entrypoint, config parsing, and startup wiring. Core application code lives under `internal/`: `db` for SQLite access, `library` for scanning and `ffprobe` metadata, `server` for HTTP handlers and HTMX/SSE flows, and `transcode` for ffmpeg job execution. Runtime templates live in `web/templates`; disk-served static assets in `web/static` (including the Vite build output at `web/static/dist`, which is gitignored and built, not checked in). The frontend toolchain (Vite + TypeScript + Alpine.js islands + PostCSS) lives in `web/frontend` — `src/main.ts` is the entry point, `src/styles/` holds the ported design-system CSS, and per-feature TS modules (`queue.ts`, `dock.ts`, `libmenu.ts`, `treesearch.ts`, `ui/resizer.ts`) pair with Vitest specs. Top-level `assets/` and `static/` are documentation and branding assets, not application runtime code. Keep long-form planning notes in `docs/plans` (completed plans move to `docs/plans/completed/`); repository automation state under `.ralphex/` is not product code.

## Build, Test, and Development Commands
Use the `Makefile` targets first:

- `make build` builds the `alto` binary from `./cmd/alto`; depends on `frontend-build` so the Vite bundle is always current.
- `make frontend-build` runs `npm ci && npm run build` in `web/frontend`, producing `web/static/dist` plus its manifest.
- `make dev` runs the Vite dev server and `go run ./cmd/alto` together (`ALTO_VITE_DEV=1`) for local frontend development with HMR.
- `make test` runs `go test ./...` across all packages.
- `make lint` runs `golangci-lint run`.
- `ALTO_LIBRARIES="Music:/path/to/music" make run` starts the app locally (serves the last built frontend bundle, not Vite HMR — use `make dev` for that).
- `make docker-build` builds a local Docker image as `alto:latest`.
- `make image-build` builds and pushes a multi-arch image using `build.sh`; defaults are `semsemyonoff/alto:latest` and `linux/amd64,linux/arm64`.
- `docker compose up -d` runs the checked-in development stack after you update the bind mounts in `docker-compose.yml` for your machine.
- Inside `web/frontend`: `npm run build`, `npm run dev`, and `npm run test` (Vitest) drive the frontend directly.

Local development expects Go `1.26.2`, Node.js (for the `web/frontend` Vite/TypeScript toolchain), plus `ffmpeg` and `ffprobe` on `PATH`. Docker is required only for container workflows.

## Coding Style & Naming Conventions
Follow standard Go formatting with `gofmt`; do not hand-format Go files to match the global EditorConfig spacing. Use lower-case package names, `CamelCase` for exported identifiers, and descriptive handler/test names such as `TestHandleTree_InvalidID`. For non-Go files, `.editorconfig` sets 4-space indentation by default, 2 spaces for `yml`, `yaml`, `json`, and `sh`, and tabs for `Makefile`.

## Testing Guidelines
Keep tests next to the code they cover in `*_test.go` files. Prefer table-driven tests and `t.TempDir()` or in-memory SQLite for isolated filesystem and DB coverage. When changing env parsing or startup behavior, extend tests under `cmd/alto`; when changing HTTP or UI behavior, extend the server tests in `internal/server`; when changing scanning or transcoding logic, add cases under `internal/library` or `internal/transcode`. Run `make test` and `make lint` before opening a PR.

## Commit & Pull Request Guidelines
Recent history uses Conventional Commit prefixes, mainly `feat:` and `fix:`, with imperative summaries like `feat: transcoding API with job management`. Keep commits focused and similarly formatted. PRs should explain behavior changes, note any environment or mount-mode implications, link related issues, and include screenshots for template/CSS/UI changes. List the verification commands you ran in the PR description.

## Configuration Tips
`ALTO_LIBRARIES` is required and uses `name:path` pairs such as `Music:/music`. Library names must match `[a-zA-Z0-9_-]`, and library names and paths must be unique. `ALTO_OUTPUT_DIR` defaults to `/out`; if you point it inside a library root, the app will warn and exclude that directory from scans. `ALTO_CACHE_DIR` should stay outside library mounts. Read-only mounts are safe only for shared `/out` mode; local `.alto-out/` and replace mode require writable library mounts. `ALTO_TRANSCODE_WORKERS` sets the bounded transcode worker-pool size (default `1`, minimum `1`); jobs beyond capacity sit `queued` until a worker frees up.

## Transcode Queue API
Transcode jobs run through a bounded worker pool with `queued`/`running`/`done`/`failed`/`canceled` states. `GET /api/jobs` lists all jobs (queue order); `GET /api/jobs/events` is the global SSE stream the queue UI subscribes to; `POST /api/jobs/{id}/cancel` cancels a queued or running job. `GET /api/transcode/{jobID}/log` still serves per-job logs, but the old per-job `GET /api/transcode/{jobID}/progress` SSE endpoint has been retired in favor of the global `/api/jobs/events` stream.

`GET /api/tree/{libraryID}/search?q=` returns a flat, library-scoped, case-insensitive list of matching directories (backed by `db.GetDirectorySearch`, paired with `web/frontend/src/treesearch.ts`).
