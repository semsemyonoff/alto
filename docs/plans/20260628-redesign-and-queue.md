# ALTO Redesign + Transcode Queue Rework

## Overview
Two coordinated changes that ship the agreed "warm hi-fi" UI redesign and the backend it needs:

1. **Backend — real transcode queue.** Today there is no queue: `runJob` (`internal/server/jobs.go:215`) spawns goroutines immediately with unbounded parallelism and only `running/done/failed` states. Introduce a bounded worker pool (`ALTO_TRANSCODE_WORKERS`, default 1), `queued` + `canceled` statuses, an ordered job list, job cancellation, a list endpoint, and a single global SSE stream so the bottom queue panel works app-wide and survives page reloads within a session.
2. **Frontend — hybrid render + real toolchain.** Keep server-rendered Go templates + HTMX for server-driven swaps (lazy tree, directory page). Add a proper frontend build (Vite + TypeScript), drive interactive "islands" with Alpine.js, and port the prototype's bespoke CSS through PostCSS. The bottom queue + SSE client is a typed TS module.

The redesign comes from the approved Claude Design prototype; a decoded local copy lives at `docs/prototype/ALTO-prototype.html` (palette, fonts, CSS variables, all three screen states).

**Problem solved:** the current UI is "purely technical"; the redesign makes ALTO feel like an apparatus (tube-amp metaphor) and the queue rework makes concurrent transcoding predictable, cancelable, and visible everywhere.

## Context (from discovery)
- **Routes:** `internal/server/server.go:233` (`registerRoutes`). Go 1.26 `net/http` mux.
- **Server/engine:** `Server.engine TranscodeEngine` (`server.go:125`); `New()` (`server.go:137`) passes `nil`, `NewWithEngine()` (`server.go:142`) injects a real engine. `handleTranscodeStart` guards `if s.engine == nil` and calls `runJob(js, s.jobs, s.engine, job, s.shutdownCtx)` (`handlers_transcode.go:158`). `s.jobs = newJobManager()` (`server.go:159`).
- **Templates:** `web/templates/` — `base.html` defines `{{define "base"}}` and references `{{template "content" .}}`. `index.html` defines `"sidebar"`, `"content"`, and `"index.html"` (= `{{template "base" .}}`). `directory.html` defines a single `"directory.html"` — a **standalone full page** that does NOT use `base`/`content`. The engine `templateEngine.render` (`handlers_pages.go:30`) does one cached `template.ParseGlob("*.html")` with **no `Funcs`**, then `ExecuteTemplate(name)`. ⚠️ Two templates defining `"content"` in one glob set collide, and funcs must be registered before parse.
- **Static (disk-served):** `web/static/css/style.css` (~21KB), `web/static/js/htmx.min.js` (referenced by templates), `web/static/js/htmx-ext-sse.min.js` (already unreferenced), `web/static/js/layout.js` (sidebar resizer), `web/static/logo.svg`. Served via `http.FileServer(http.Dir(s.staticDir))` — **NOT** `embed.FS`. Dockerfile copies `web/`.
- **Job manager:** `internal/server/jobs.go` — `jobManager{mu, jobs, byDir}`, `jobState` with `latest` guarded by `subsMu`, per-job SSE fanout (`subscribe/subs/broadcast/closeSubs`), 30-min eviction in `complete()`, dir dedup `byDir`. `runJob` spawns a fanout goroutine (drains `js.progress` → `latest` + logs) **and** an engine goroutine.
- **Transcode types:** `progress.go` — `ProgressReport{CurrentFile, FileIndex, TotalFiles, FilePercent}` (⚠️ **no** `OverallPercent`; overall is `calcOverallPercent(p)` at `handlers_transcode.go:301`). `presets.go` — `DefaultPresets()` (FLAC Fast/Balanced/Max; Opus Music Balanced 128k / Music High 160k / Archive Lossy 192k).
- **Scanner:** `internal/library/scanner.go` implements `LibraryScanner.ScanAll(ctx, libs) error` (`server.go:18`); scans libraries **concurrently**; no progress hook today.
- **`GET /api/libraries`** already exists; returns `{libraries:[{ID,Name,Path}]}`.
- **Test helpers:** `newTestServer` (via `New()`, nil engine), `newTestServerWithRealTemplates`, `newTestServerWithEngine` — each only `t.Cleanup(database.Close())`, no `srv.Shutdown()`.

### Deviations from the brainstorm assumptions (ratified during discovery)
- **No `embed.FS`.** Vite builds into `web/static/dist/`, served by the existing FileServer. Single-binary embedding is out of scope for v1.
- **`/api/libraries` is extended, not added.**
- **Presets are the real `transcode.DefaultPresets()`.**
- **`directory.html` is unified onto `base.html`** (Task 13) so the shell, Vite assets, and global queue exist on every screen.

## Development Approach
- **Testing approach: Regular** (implement, then write tests in the same task).
- **Each task must keep package `server` compiling and all tests green** — caller and callee signature changes go in the same task; no task may leave a broken build or a non-working primary flow (e.g. transcode progress UI) for a later task.
- **Every task includes new/updated tests** (success + error/edge), as separate checklist items. Backend tests run under `-race`.
- Backend: Go table-driven, `t.TempDir()` / in-memory SQLite, beside code in `*_test.go`.
- Frontend logic (SSE/queue client, dock codec/preset, resizer): **Vitest**.
- Pure visual/CSS tasks: verified by `npm run build` + Go template-render tests where a handler shapes data; subjective visual QA in Post-Completion.
- Run `make test`, `make lint`, `npm run build` before each PR. Keep this plan synced.

## Testing Strategy
- **Unit (Go, -race):** job state transitions; worker-pool sequencing (workers=1 serializes, workers=2 both run, no stranded queued work); worker exit on shutdown (no leak); cancel queued/running + terminal retention + eviction; event-bus ordering + atomic subscribe+snapshot + slow-subscriber drop; `latest` access race-free; endpoint handlers (status + JSON/SSE); DTO shaping; scan progress aggregation; env parsing.
- **Unit (Vitest):** queue/SSE client (reconcile add/update/complete/cancel, count derivation, cancel URL); dock codec/preset filtering + request body; resizer clamp/persist; Alpine `htmx:afterSwap` re-init.
- **Template-render (Go):** new templates via `httptest`; assert a test-safe asset resolver works without a committed `dist`.
- **No e2e framework**; cross-screen flows are manual scenarios in Post-Completion.

## Progress Tracking
- Mark items `[x]` immediately. New tasks `➕`; blockers `⚠️`. Keep synced.

## Solution Overview
- **Queue (concurrency model — single mutex).** `jobManager` holds `jobs map[string]*jobState`, ordered `order []string` (the single source of truth for the queue list — terminal jobs stay until eviction), `byDir`, `engine TranscodeEngine`, `workers int`, one `sync.Mutex mu` with a `sync.Cond` on it, a worker `WaitGroup`, and a process-wide event subscriber set. **`mu` guards ALL job state — `status`, `latest`, `errMsg`, `order`, AND both the per-job and global SSE subscriber lists** (the old `subsMu` is collapsed into `mu` from the start, eliminating any AB/BA lock ordering). Every SSE send (per-job and global) is a **non-blocking buffered-channel send with default-drop**, so it is safe to perform under `mu`; the HTTP handlers write their responses by reading their own channel **outside** the lock.
- `start()` registers a job `queued`, stores its `transcode.Job` on the `jobState`, appends to `order`, `cond.Broadcast()`. Each worker loops: under `mu` find the first `queued` id, transition it `running`, build its cancel ctx; **unlock**, then `runOneJob` (below) blocks until the engine returns AND the fanout goroutine has fully drained `js.progress` (so `latest`/logs are final); relock, `complete()` (emit terminal event with finalized pct); repeat; when no `queued` work remains, `cond.Wait()`. A worker thus **occupies its slot for the whole job**, preserving bounded concurrency.
- `runOneJob` (replaces `runJob`, worker-owned, synchronous): start the fanout goroutine (drains `js.progress` → updates `latest`/logs under `mu`, emits a global `update` per report) and the engine goroutine; wait for the engine result, `close(js.progress)`, wait on `fanoutDone`, return the error to the worker. Never holds `mu` while waiting.
- Workers exit when a shutdown flag is set (Shutdown cancels `shutdownCtx`, sets the flag, `cond.Broadcast()`, then `wg.Wait()`). **The pool starts only when `engine != nil`** (so `New()`-based tests spawn no workers). `pct` is shaped centrally (see Technical Details), not read raw.
- **Terminal retention.** `complete()` and queued-`cancel()` both: set the terminal status, free `byDir`, close `done`, emit an event, and schedule a single 30-min eviction that removes the id from **both** `jobs` and `order`. Done/failed/canceled jobs therefore remain visible in `/api/jobs` until eviction; the dispatcher only ever picks `queued` ids.
- **Frontend.** Vite builds TS+CSS into `web/static/dist/` with a hashed manifest. `templateEngine` is reworked to register asset FuncMaps and parse `base.html` **+ shared partials (incl. a new `sidebar` partial) + the requested page** via `ParseFiles` (cached per page name) — isolating each page's `content` block while keeping `sidebar`/shell available, and fixing func-before-parse. An explicit asset-resolver mode (prod manifest / `ALTO_VITE_DEV` dev / test stub) is selected by config, not by silent fallback, so a missing prod manifest fails loudly. `directory.html` is refactored to render through `base.html`. `main.ts` re-inits Alpine on `htmx:afterSwap`. Alpine powers dock controls, dropdowns, toggles, library menu, log expanders; a TS module owns the global queue + SSE.

## Technical Details
- **`JobStatus`:** add `JobStatusQueued = "queued"`, `JobStatusCanceled = "canceled"`.
- **`jobState` new fields:** `title`, `sub`, `job transcode.Job`, `cancel context.CancelFunc`, `fanoutDone chan struct{}`. All mutable job state guarded by the single `jobManager.mu`.
- **`jobManager` new fields:** `order []string`, `engine`, `workers int`, `cond *sync.Cond`, `wg sync.WaitGroup`, `shutdown bool`, event subscriber set. `subsMu` is removed — `mu` guards per-job + global subscribers too. `newJobManager(engine, workers, parentCtx)`; start pool only if `engine != nil`.
- **Worker count:** `ALTO_TRANSCODE_WORKERS` int, default 1, min 1.
- **pct shaping (centralized DTO helper):** `status == done` → `pct = 100`; `running` → `calcOverallPercent(*latest)` (0 if no report yet); `failed`/`canceled` → last known pct or 0. Used by both `/api/jobs` and every `update` event so a finished job never shows a partial meter.
- **`/api/jobs`:** `{"jobs":[{"id","title","sub","pct","status"}]}` in `order`.
- **`/api/jobs/events` (SSE):** `subscribeEventsWithSnapshot()` registers the subscriber and builds the snapshot **under one `mu` lock** (no TOCTOU), then the handler streams snapshot events followed by live deltas (event `update`, JSON `{id,status,pct,title,sub}`) by reading its channel **outside** the lock; sends into the channel are non-blocking, slow subscribers dropped.
- **`POST /api/jobs/{id}/cancel`:** queued → set status `canceled` (**do NOT remove from `order`**; dispatcher only ever picks `queued`, so it is skipped), free `byDir`, close `done`, emit, schedule the shared eviction; running → stored `cancel()` (engine ctx error → `complete()` maps to `canceled`); unknown/finished → 404/409.
- **Retire (Task 18, after the new queue UI works):** `GET /api/transcode/{jobID}/progress` route + `handleTranscodeProgress`, the per-job subscriber machinery (`subscribe/subs/unsubscribe/closeSubs`), and the directory page's per-job `EventSource`. **Keep** `GET /api/transcode/{jobID}/log`.
- **`libraryDTO`:** add `Indexed bool`, `TrackCount int` (`indexed = TrackCount > 0`).
- **`dirPageData`:** add `TotalDuration`, `TotalSize` (summed in `handleDirPage`).
- **Presets to template:** `DefaultPresets()` grouped by codec → JSON `<script type="application/json">`.
- **Scan progress:** extend `ScanAll` with a **plain func callback** `progress func(libraryID int64, discoveredDirs int)` (no server-owned named type → no `library`→`server` import cycle); `internal/library` invokes it per library; the server aggregates concurrent libraries under a mutex and emits a scan SSE `progress` event; bar indeterminate. All scanner test doubles (`mockScanner`/`scannerFunc`) update to the new signature in the same task.
- **Asset template funcs:** `viteTags(entry)` returns `template.HTML` (`@vite/client` + module/link tags in dev; hashed `<script>/<link>` from the manifest in prod) for the head; optional `viteAsset(path)` returns a single hashed URL for a static asset. Resolver mode is chosen explicitly (prod / `ALTO_VITE_DEV` / test-stub via the server/test constructor), and prod errors loudly if `web/static/dist/.vite/manifest.json` is absent.

## What Goes Where
- **Implementation Steps** (`[ ]`): code, tests, templates/CSS/TS, build wiring, docs.
- **Post-Completion** (no checkboxes): visual QA, manual cross-screen scenarios, image/perf verification.

## Implementation Steps

### Task 1: Extend job state — statuses, metadata, job/cancel handles, centralize locking

**Files:**
- Modify: `internal/server/jobs.go`
- Modify: `internal/server/jobs_test.go`

- [x] add `JobStatusQueued`, `JobStatusCanceled`; add `title`, `sub`, `job transcode.Job`, `cancel context.CancelFunc`, `fanoutDone chan struct{}` to `jobState`
- [x] **collapse `subsMu` into `jobManager.mu`**: all per-job state (`latest`/`status`/`errMsg`) and the per-job subscriber list move under `mu`; per-job `broadcast`/`subscribe` use `mu` with non-blocking sends (one lock, no AB/BA ordering)
- [x] map a context-canceled engine error to `JobStatusCanceled` in `complete()`
- [x] write tests for status mapping (done/failed/canceled) + metadata persistence + race-free `latest` read/write under one mutex
- [x] run `go test -race` — must pass before next task

### Task 2: Worker pool dispatcher (sync.Cond) + caller update + preserved fanout

**Files:**
- Modify: `internal/server/jobs.go`
- Modify: `internal/server/server.go` (`newJobManager(engine, workers, shutdownCtx)`; pass engine + worker count)
- Modify: `internal/server/handlers_transcode.go` (`handleTranscodeStart` enqueues via new `start`, no direct `runJob`)
- Modify: `internal/server/jobs_test.go`, `internal/server/transcode_test.go` (helper `t.Cleanup(srv.Shutdown)`)

- [x] add `order`, `engine`, `workers`, `cond` (on `mu`), `wg`, `shutdown` to `jobManager`; `newJobManager` starts N workers **only if `engine != nil`**
- [x] replace async `runJob` with a **synchronous worker-owned `runOneJob`**: start fanout + engine goroutines, wait for the engine result, `close(js.progress)`, wait on `fanoutDone` (so `latest`/logs are finalized), return err — never holding `mu` while waiting
- [x] worker loop: under `mu` pick first `queued` id, transition `running`, build cancel ctx, **unlock**, call `runOneJob` (slot occupied for the whole job → bounded concurrency), relock, `complete()` (emit terminal event with finalized pct); when none queued `cond.Wait()`; exit on `shutdown`
- [x] change `start(...)` to register `queued`, store `Job`/`title`/`sub`, keep `byDir` dedup, append to `order`, `cond.Broadcast()`; update `handleTranscodeStart` to call it (title = dir basename or `library/relpath`; sub = `sourceCodec → preset`)
- [x] add `Shutdown` path: set flag, cancel ctx, `cond.Broadcast()`, `wg.Wait()`; engine-based test helpers get `t.Cleanup(srv.Shutdown)`; default 1 / min 1 workers
- [x] write tests: workers=1 serializes (2nd stays queued then auto-starts); workers=2 run two concurrently with **no stranded queued job**; a job marked complete only after fanout drains (no progress event after terminal); `order` preserved; workers exit on shutdown (no leak under `-race`)
- [x] run tests — must pass before next task

### Task 3: Cancellation + consistent terminal retention/eviction

**Files:**
- Modify: `internal/server/jobs.go`
- Modify: `internal/server/jobs_test.go`

- [x] add `cancel(id)`: queued → set status `canceled` (**keep the id in `order`**; dispatcher skips non-`queued`), free `byDir`, close `done`, emit, schedule the shared 30-min eviction; running → invoke stored `cancel()`; return typed result (canceled / not-found / finished)
- [x] unify eviction so `complete()` and queued-cancel both remove the id from **both `jobs` and `order`** after 30 min; dispatcher only picks `queued`
- [x] write tests: cancel queued (stays listed as `canceled`, never starts, then evicted from both maps), cancel running (ctx fires → canceled), cancel unknown/finished, done/failed jobs remain listed until eviction
- [x] run tests — must pass before next task

### Task 4: Global job event bus (atomic subscribe+snapshot)

**Files:**
- Modify: `internal/server/jobs.go`
- Modify: `internal/server/jobs_test.go`

- [x] add a process-wide subscriber set guarded by `mu`; emit `update {id,status,pct,title,sub}` on enqueue/start/progress/complete/cancel; add the centralized pct-shaping helper (done→100, running→calcOverallPercent, failed/canceled→last/0)
- [x] add `subscribeEventsWithSnapshot()` that, under one `mu` lock, registers the subscriber and captures the `order` snapshot (no TOCTOU); non-blocking broadcast, drop slow subscribers; handler writes outside the lock
- [x] keep the existing per-job SSE working for now (retired in Task 18), now sharing the same `mu`
- [x] write tests: events in order; snapshot+subscribe atomic (no missed update); slow subscriber dropped, never blocks; pct shaping (done shows 100 even with no final report)
- [x] run tests — must pass before next task

### Task 5: `GET /api/jobs` list endpoint

**Files:**
- Modify: `internal/server/handlers_transcode.go`, `internal/server/server.go`, `internal/server/handlers_transcode_test.go`

- [x] add `handleJobs` returning `{"jobs":[...]}` from the snapshot in `order` using the centralized pct-shaping helper (done→100 etc.)
- [x] register `GET /api/jobs`
- [x] write tests for empty + mixed-status (incl. terminal) lists (JSON shape + order + done-at-100)
- [x] run tests — must pass before next task

### Task 6: `GET /api/jobs/events` global SSE (additive; old progress kept)

**Files:**
- Modify: `internal/server/handlers_transcode.go`, `internal/server/server.go`, `internal/server/handlers_transcode_test.go`

- [x] add `handleJobEvents`: SSE headers, `subscribeEventsWithSnapshot`, replay snapshot then stream `update`, unsubscribe on disconnect
- [x] register `GET /api/jobs/events`; **do not** remove the per-job progress endpoint yet (kept working until Task 18)
- [x] write tests: SSE replays snapshot then a live delta; disconnect unsubscribes (no leak under `-race`)
- [x] run tests — must pass before next task

### Task 7: `POST /api/jobs/{id}/cancel` endpoint

**Files:**
- Modify: `internal/server/handlers_transcode.go`, `internal/server/server.go`, `internal/server/handlers_transcode_test.go`

- [x] add `handleJobCancel` mapping `cancel()` results to 202 / 404 / 409
- [x] register `POST /api/jobs/{id}/cancel`
- [x] write tests for each response path
- [x] run tests — must pass before next task

### Task 8: Wire `ALTO_TRANSCODE_WORKERS` config

**Files:**
- Modify: `cmd/alto/main.go` (+ int env helper), `internal/server/server.go` (`Config.Workers` → `newJobManager`), `cmd/alto/*_test.go`

- [x] add `Config.Workers` + an int env parse helper (default 1, min 1, invalid → error/clamp consistent with existing env handling)
- [x] parse `ALTO_TRANSCODE_WORKERS`, thread config → `Server`/`NewWithEngine` → `newJobManager`
- [x] write tests for env parsing (default/valid/invalid)
- [x] run tests — must pass before next task

### Task 9: Scaffold Vite + TS frontend (Alpine, HTMX, Vitest, HTMX re-init)

**Files:**
- Create: `web/frontend/package.json`, `vite.config.ts`, `tsconfig.json`, `postcss.config.js`, `src/main.ts`, `src/main.test.ts`
- Modify: `.gitignore`

- [ ] init npm with `vite`, `typescript`, `alpinejs`, `htmx.org`, `postcss`, `postcss-nesting`, `autoprefixer`, `vitest`
- [ ] Vite config: `build.manifest=true`, `outDir=../static/dist`, `base=/static/dist/`, main entry input
- [ ] `main.ts`: bootstrap Alpine + HTMX, import CSS entry, **re-init Alpine on `htmx:afterSwap`**
- [ ] add `build`/`dev`/`test` scripts; gitignore `node_modules` and `web/static/dist`
- [ ] write a Vitest test for the `htmx:afterSwap` re-init helper (+ sanity)
- [ ] run `npm run build` and `npm run test` — must pass before next task

### Task 10: templateEngine rework + Vite manifest integration (test-safe)

**Files:**
- Modify: `internal/server/handlers_pages.go` (templateEngine)
- Create: `internal/server/assets.go`, `internal/server/assets_test.go`, `web/templates/sidebar.html` (extract `{{define "sidebar"}}` out of `index.html`)
- Modify: `web/templates/base.html`, `web/templates/index.html` (drop its local `sidebar` def), `internal/server/server.go`

- [ ] extract the `sidebar` block into a shared `sidebar.html` partial so both pages get it once `directory.html` stops duplicating the shell
- [ ] rework `templateEngine`: register asset funcs (`viteTags`, `viteAsset`) **before** parsing; parse `base.html` + shared partials (`sidebar.html`) + the requested page via `ParseFiles`, caching one set per page name (isolates each page's `content`, keeps `sidebar` available)
- [ ] asset resolver with an **explicit mode** (prod manifest / `ALTO_VITE_DEV` dev / test-stub via constructor): `viteTags("src/main.ts")` returns `template.HTML` head tags; prod **fails loudly** if the manifest is missing; test-stub returns placeholders so real-template Go tests pass without a built `dist`
- [ ] use `{{ viteTags "src/main.ts" }}` in `base.html` head
- [ ] write tests: manifest parse → hashed tags; dev mode emits `@vite/client`; prod missing-manifest errors; test-stub renders; existing real-template tests still pass; `/` and `/dir` both render `sidebar` without collision
- [ ] run tests + `npm run build` — must pass before next task

### Task 11: Build pipeline + retire unreferenced asset

**Files:**
- Modify: `Makefile`, `Dockerfile`
- Create: `.dockerignore` (if absent)
- Delete: `web/static/js/htmx-ext-sse.min.js` (already unreferenced)

- [ ] `make frontend-build` (`npm ci && npm run build`), `make dev` (Vite dev + `go run`), make `build` depend on `frontend-build`
- [ ] Dockerfile Node build stage (`npm ci && npm run build`) producing `web/static/dist` before `go build`; `.dockerignore` excludes `node_modules`, keeps `dist`
- [ ] remove the already-unreferenced `htmx-ext-sse.min.js` (vendored `htmx.min.js` and `layout.js` are removed later, when their template references are gone — Tasks 14/15)
- [ ] smoke check: `make build` yields a binary + non-empty `web/static/dist/.vite/manifest.json`
- [ ] run `make build` — must pass before next task

### Task 12: Port design-system CSS (tokens, base, typography)

**Files:**
- Create: `web/frontend/src/styles/tokens.css`, `base.css`, `index.css`
- Modify: `web/frontend/src/main.ts`

- [ ] extract `:root` vars, fonts (Space Grotesk / Manrope / IBM Plex Mono), body background, resets from `docs/prototype/ALTO-prototype.html`
- [ ] confirm accents match `static/logo.svg` (green `#6EDD81`, teal `#3AD4C4`, cyan `#1EC2EC`)
- [ ] import CSS index from `main.ts`; verify PostCSS nesting + autoprefixer run; hashed CSS in manifest
- [ ] run `npm run build` — must pass before next task

### Task 13: Unify `directory.html` onto the `base.html` shell

**Files:**
- Modify: `web/templates/directory.html` (define `content`, render through `base`), `internal/server/handlers_pages.go` (render dir page via per-page parse from Task 10)
- Modify: `internal/server/handlers_pages_test.go`, `internal/server/directory_test.go`

- [ ] convert `directory.html` to define the `content` block (dir stage) and reuse `base`'s head/topbar/queue and the shared `sidebar` partial; drop its duplicated `<head>/<body>` + scan-SSE copy (relies on Task 10's per-page parse + `sidebar.html`, so `content` no longer collides and `sidebar` is present on `/dir`)
- [ ] preserve the swap contract: direct `/dir` renders full page; tree click still HTMX-swaps `#dir-content` (`hx-select` unchanged)
- [ ] write tests: `/dir` full page includes shell ids (topbar, queue), the rendered `sidebar`, + `#dir-content`; HTMX-select region intact; both pages render without template collision
- [ ] run tests + `npm run build` — must pass before next task

### Task 14: Rebuild app shell — topbar, grid, seam (+ htmx via bundle)

**Files:**
- Modify: `web/templates/base.html`
- Create: `web/frontend/src/styles/shell.css`
- Delete: `web/static/js/htmx.min.js` (now bundled via npm)

- [ ] rebuild `base.html` shell (`.shell` grid, `.topbar`, status dot, Re-index, `.seam`) with prototype markup/classes; HTMX now loaded via the Vite bundle, so remove the vendored `htmx.min.js` reference + file
- [ ] move shell styles to `shell.css`; keep scan-status SSE working against new ids
- [ ] write a Go template-render test asserting shell ids/blocks
- [ ] run tests + `npm run build` — must pass before next task

### Task 15: Rebuild tree sidebar (+ resizer in TS)

**Files:**
- Modify: `web/templates/index.html`, `internal/server/handlers_pages.go` (tree fragment markup)
- Create: `web/frontend/src/styles/tree.css`, `web/frontend/src/ui/resizer.ts`, `web/frontend/src/ui/resizer.test.ts`
- Delete: `web/static/js/layout.js`
- Modify: `internal/server/handlers_pages_test.go`

- [ ] restyle tree (twisties, icons, codec badges, selection, search input) with prototype classes; update `handleTreeChildren` fragment markup keeping HTMX lazy attrs
- [ ] reimplement the sidebar resizer in `resizer.ts` (localStorage persistence) and remove `layout.js`
- [ ] write Go tests for fragment markup (classes, HTMX attrs, badge) + Vitest test for resizer clamp/persist
- [ ] run tests + `npm run build` — must pass before next task

### Task 16: Rebuild directory stage + album aggregates

**Files:**
- Modify: `web/templates/directory.html`, `internal/server/handlers_pages.go` (`dirPageData` + `handleDirPage`)
- Create: `web/frontend/src/styles/stage.css`
- Modify: `internal/server/handlers_pages_test.go`

- [ ] add `TotalDuration`/`TotalSize` to `dirPageData`, summed in `handleDirPage`
- [ ] rebuild stage markup (cover, crumbs, title, codec pill, subline, track table); "show technical" toggle as Alpine island toggling `.hide-tech`
- [ ] write tests for aggregates + stage render (codec pill class, toggle markup)
- [ ] run tests + `npm run build` — must pass before next task

### Task 17: Transcode dock as Alpine island

**Files:**
- Modify: `web/templates/directory.html`, `internal/server/handlers_pages.go` (presets JSON + `CanTranscode`)
- Create: `web/frontend/src/dock.ts`, `web/frontend/src/dock.test.ts`, `web/frontend/src/styles/dock.css`

- [ ] marshal `DefaultPresets()` grouped by codec into a JSON `<script>`; render dock markup (codec toggle, preset dropdown, output modes incl. replace warning, START)
- [ ] Alpine component: codec switch filters presets, output-mode selection, builds `POST /api/transcode` body; START disabled with reason when `CanTranscode` is false (keep request shape compatible with `handleTranscodeStart`)
- [ ] write Vitest tests for preset filtering + request-body build (incl. disabled-on-lossy)
- [ ] run `npm run test` + `make test` — must pass before next task

### Task 18: Global queue panel (TS + SSE) and retire per-job progress

**Files:**
- Modify: `web/templates/base.html` (queue markup), `web/templates/directory.html` (remove old `EventSource`)
- Modify: `internal/server/handlers_transcode.go`, `internal/server/server.go`, `internal/server/jobs.go` (remove dead per-job subscriber machinery)
- Create: `web/frontend/src/queue.ts`, `web/frontend/src/queue.test.ts`, `web/frontend/src/styles/queue.css`
- Modify: `internal/server/handlers_transcode_test.go`

- [ ] TS module: initial `GET /api/jobs`, subscribe `GET /api/jobs/events`, reconcile rows (VU fill/needle, pct, status dot/label), bubble counts; row click expands logs (lazy `GET /api/transcode/{id}/log`); `×` → `POST /api/jobs/{id}/cancel`; panel collapse
- [ ] now that the queue UI is live, **retire** `GET /api/transcode/{jobID}/progress` (route + `handleTranscodeProgress`), remove the directory page's old `EventSource`, and delete the now-dead `subscribe/subs/unsubscribe/closeSubs` machinery (keep `latest` maintained by the fanout)
- [ ] render VU/queue styles into `queue.css`
- [ ] write Vitest tests (reconcile add/update/complete/cancel, counts, cancel URL) + Go test confirming the old progress route is gone and `/log` still works
- [ ] run `npm run test` + `make test` — must pass before next task

### Task 19: Library selector + indexed status

**Files:**
- Modify: `internal/server/handlers_api.go` (`libraryDTO` + `handleLibraries`), `internal/server/handlers_api_test.go`, `internal/db/db.go` (per-library track count), `web/templates/base.html` (library menu)
- Create: `web/frontend/src/libmenu.ts`

- [ ] add a per-library track-count query; extend `libraryDTO` with `Indexed`/`TrackCount`; populate in `handleLibraries`
- [ ] rebuild the topbar library selector + dropdown (Alpine) showing count / "not indexed"; switching loads its tree (HTMX) + updates the status dot
- [ ] write Go tests for the DTO (indexed true/false, counts) + count query; Vitest test for menu state if non-trivial
- [ ] run tests + `npm run build` — must pass before next task

### Task 20: Standby / empty / scan-progress (+ scanner progress hook)

**Files:**
- Modify: `internal/server/server.go` (`LibraryScanner` interface + scan SSE `progress`), `internal/library/scanner.go` (+ test) — progress callback
- Modify: scanner test doubles in `internal/server/*_test.go` (`mockScanner`/`scannerFunc`) for the new signature
- Modify: `internal/server/handlers_pages.go` (render standby when not indexed), `web/templates/index.html` / `directory.html` (standby + scan markup)
- Create: `web/frontend/src/styles/standby.css`
- Modify: scan-related server test

- [ ] extend `ScanAll` with a plain `progress func(libraryID int64, discoveredDirs int)` param (no shared named type → no import cycle); `internal/library` calls it per library; aggregate concurrent libraries under a mutex in the server; emit a scan SSE `progress` event; bar indeterminate; update all scanner test doubles
- [ ] render the standby/empty state (gauge + "Re-index to scan") when the selected library has no indexed directories
- [ ] wire the scan animation (indeterminate bar + live count) to the scan SSE
- [ ] write tests: scanner reports increasing counts (race-free under parallel scan); SSE emits `progress`; standby renders for an un-indexed library
- [ ] run tests + `npm run build` — must pass before next task

### Task 21 (optional): Server-side tree search

**Files:**
- Modify: `internal/server/handlers_pages.go` (or `handlers_api.go`), `internal/server/server.go`, `web/templates/index.html` / `web/frontend/src/`, corresponding `*_test.go`

- [ ] add `GET /api/tree/{libraryID}/search?q=` → flat list of matching dirs (case-insensitive contains, library-scoped); wire the search input + render results with highlight
- [ ] write tests (match, no-match, empty query, scoping)
- [ ] run tests — must pass before next task
- [ ] NOTE: optional for v1; nothing else depends on it — skip if descoping.

### Task 22: Verify acceptance criteria
- [ ] verify Overview requirements (queue: pool/queued/cancel/list/global SSE; UI: shell/tree/stage/dock/queue/standby/scan; one shell on every screen)
- [ ] verify edge cases: cancel queued vs running + terminal retention, workers=1 vs 2 with no stranded jobs, worker exit on shutdown, lossy disables START, un-indexed standby, Alpine survives HTMX swap
- [ ] run `make test` (with `-race`) and `make lint`
- [ ] run `cd web/frontend && npm run test && npm run build`

### Task 23: [Final] Update documentation
- [ ] update `CLAUDE.md` (`ALTO_TRANSCODE_WORKERS`; frontend toolchain + `make dev`/`make frontend-build`; `web/frontend` layout; Node prerequisite; retired `/api/transcode/{id}/progress`, new `/api/jobs*`)
- [ ] update `README.md` if it documents env/build/run
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems — informational only.*

**Manual verification:**
- Visual QA of all three screens against `docs/prototype/ALTO-prototype.html`.
- Cross-screen flows: select dir → START → encoding row → second START queues → first completes → second auto-starts; cancel queued; cancel running; reload mid-encode → queue repopulates from `/api/jobs`; navigate index↔directory and confirm dock/queue/Alpine still work after HTMX swaps.
- Standby → Re-index → scan animation with live count → populated tree.
- Lossy (MP3) directory shows START disabled with a reason.
- `prefers-reduced-motion` disables VU/spinner animations.

**External / build verification:**
- `make image-build` multi-arch image builds with the Node stage and runs (ffmpeg present, assets from `web/static/dist`).
- Sanity with ~20 queued jobs at `ALTO_TRANSCODE_WORKERS=1` (and a run at 2) — ordering, no stranded jobs, memory, SSE stable.
- `make dev` serves templates with Vite HMR for CSS/TS.
