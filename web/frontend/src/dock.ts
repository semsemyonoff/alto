/**
 * The transcode dock: an Alpine island rendered beside a directory's track
 * table. It filters transcode.DefaultPresets() (marshaled server-side into a
 * per-page <script id="tc-presets-data"> tag) by the selected codec, builds
 * the POST /api/transcode request body, and disables START with a reason
 * when the directory isn't transcodable. Once a job is enqueued its
 * lifecycle (progress, completion, cancellation) is tracked by the global
 * queue panel (queue.ts), not by the dock.
 */

export type Codec = 'flac' | 'opus'
/** '' means the user has not yet chosen an output destination. */
export type OutputMode = '' | 'shared' | 'local' | 'replace'

export interface Preset {
  name: string
  label: string
  default?: boolean
}

export type PresetsByCodec = Record<string, Preset[]>

/** Returns the presets available for codec, or an empty list if none are known. */
export function presetsForCodec(presets: PresetsByCodec, codec: string): Preset[] {
  return presets[codec] ?? []
}

/** Returns the name of the preset flagged `default` for its codec, falling back to the first entry. */
export function defaultPresetName(presets: Preset[]): string {
  return presets.find((p) => p.default)?.name ?? presets[0]?.name ?? ''
}

/** Parses the JSON text of the #tc-presets-data script tag, tolerating missing/invalid content. */
export function parsePresets(json: string | null | undefined): PresetsByCodec {
  if (!json) return {}
  try {
    const parsed: unknown = JSON.parse(json)
    return parsed && typeof parsed === 'object' ? (parsed as PresetsByCodec) : {}
  } catch {
    return {}
  }
}

export interface TranscodeRequestInput {
  path: string
  codec: Codec
  preset: string
  outputMode: OutputMode
  /** The dock's "Skip lossy" toggle — server-side sugar for "every lossless track". */
  skipLossy?: boolean
  /** The selected lossless filenames, in directory order. */
  files?: string[]
  /** Tracks in the directory, so a full selection can be told from a subset. */
  trackCount?: number
  /** Copy the unselected tracks into the output verbatim. */
  copySkipped?: boolean
}

export interface TranscodeRequestBody {
  path: string
  codec: string
  preset: string
  output_mode: string
  skip_lossy?: boolean
  files?: string[]
  copy_skipped?: boolean
}

/** How many of the directory's tracks the current selection leaves out. */
export function skippedCount(trackCount: number, selectedCount: number): number {
  return Math.max(0, trackCount - selectedCount)
}

/**
 * Builds the JSON body for POST /api/transcode, matching the server's
 * transcodeRequest shape.
 *
 * `skip_lossy` and `files` are mutually exclusive server-side (400
 * invalid_request), and an empty `files` is rejected outright, so at most one is
 * ever emitted. A selection covering the whole directory sends neither, keeping
 * an all-lossless album on the request shape it had before per-track selection.
 */
export function buildTranscodeRequestBody(input: TranscodeRequestInput): TranscodeRequestBody {
  const body: TranscodeRequestBody = {
    path: input.path,
    codec: input.codec,
    preset: input.preset,
    output_mode: input.outputMode,
  }
  const files = input.files ?? []
  const trackCount = input.trackCount ?? files.length
  if (input.skipLossy) {
    body.skip_lossy = true
  } else if (files.length > 0 && files.length < trackCount) {
    body.files = files
  }
  // The server refuses copy_skipped in replace mode (the originals are already
  // in place) and it is meaningless with nothing skipped, so it is only sent
  // where the checkbox is actually offered.
  if (input.copySkipped && input.outputMode !== 'replace' && skippedCount(trackCount, files.length) > 0) {
    body.copy_skipped = true
  }
  return body
}

/** The inputs both START gating and its status line are derived from. */
export interface StartState {
  /** The directory holds at least one lossless track (server-rendered). */
  canTranscode: boolean
  trackCount: number
  selectedCount: number
  starting: boolean
  outputMode: OutputMode
  activeInQueue: boolean
}

/**
 * Reports whether the dock's START control should be enabled. START requires a
 * directory with at least one selected lossless track, an explicitly chosen
 * output destination, no in-flight start, and no active (queued/running) job
 * already for this album.
 */
export function canStartTranscode(s: StartState): boolean {
  return (
    s.canTranscode &&
    s.trackCount > 0 &&
    s.selectedCount > 0 &&
    !s.starting &&
    s.outputMode !== '' &&
    !s.activeInQueue
  )
}

/** The status line shown under START — a disabled reason, or the selection size when ready. */
export function startStatusText(s: StartState): string {
  if (s.activeInQueue) return 'Already in the queue'
  if (s.starting) return 'Starting…'
  if (s.trackCount <= 0) return 'No tracks to transcode'
  if (!s.canTranscode) return 'No lossless tracks'
  if (s.selectedCount <= 0) return 'Nothing selected'
  if (s.outputMode === '') return 'Choose an output destination'
  if (s.selectedCount < s.trackCount) return `${s.selectedCount} of ${s.trackCount} tracks`
  return `${s.selectedCount} track${s.selectedCount === 1 ? '' : 's'}`
}

export interface DockConfig {
  path: string
  canTranscode: boolean
  trackCount: number
  libraryId: string
  presets: PresetsByCodec
}

/** Reads the dock's static config (path/eligibility/presets) from the DOM. */
export function readDockConfig(el: HTMLElement, doc: Document = document): DockConfig {
  const presetsEl = doc.getElementById('tc-presets-data')
  return {
    path: el.dataset.path ?? '',
    canTranscode: el.dataset.canTranscode === 'true',
    trackCount: Number(el.dataset.trackCount ?? '0'),
    libraryId: el.dataset.libraryId ?? '',
    presets: parsePresets(presetsEl?.textContent),
  }
}

interface DockData {
  codec: Codec
  presetsByCodec: PresetsByCodec
  presets: Preset[]
  preset: string
  presetOpen: boolean
  outputMode: OutputMode
  copySkipped: boolean
  starting: boolean
  error: string
  path: string
  canTranscode: boolean
  trackCount: number
  libraryId: string
  resultText: string
  resultOk: boolean
  init(): void
  readonly presetLabel: string
  readonly activeInQueue: boolean
  readonly selectedNames: string[]
  readonly selectedCount: number
  readonly losslessCount: number
  readonly lossyCount: number
  readonly hasLossy: boolean
  readonly skipLossy: boolean
  readonly skippedCount: number
  readonly showCopySkipped: boolean
  readonly showSelection: boolean
  readonly startState: StartState
  readonly canStart: boolean
  readonly statusText: string
  setSkipLossy(on: boolean): void
  setCodec(codec: Codec): void
  selectPreset(name: string): void
  start(): Promise<void>
  reindex(): void
}

/** The slice of `$store.selection` the dock reads; see selection.ts for the full store. */
interface DockSelectionStore {
  skipLossy: boolean
  losslessCount: number
  names: string[]
  setSkipLossy(on: boolean): void
}

interface DockStores {
  jobs?: { isActive(dir: string): boolean }
  selection?: DockSelectionStore
}

/** Alpine injects `$store` onto the component instance at runtime; `this` is untyped there. */
function stores(self: unknown): DockStores | undefined {
  return (self as { $store?: DockStores }).$store
}

/** Alpine.data factory registered as `altoDock` and referenced via `x-data="altoDock()"`. */
export function altoDock(): DockData {
  return {
    codec: 'flac',
    presetsByCodec: {},
    presets: [],
    preset: '',
    presetOpen: false,
    // No pre-selected destination: the user must choose one, which also keeps
    // every mode button a real toggle (clicking the would-be default is no
    // longer a no-op) and gates START until a destination is picked.
    outputMode: '',
    copySkipped: false,
    starting: false,
    error: '',
    path: '',
    canTranscode: false,
    trackCount: 0,
    libraryId: '',
    resultText: '',
    resultOk: false,

    init() {
      const el = (this as unknown as { $el: HTMLElement }).$el
      const config = readDockConfig(el)
      this.path = config.path
      this.canTranscode = config.canTranscode
      this.trackCount = config.trackCount
      this.libraryId = config.libraryId
      this.presetsByCodec = config.presets
      this.presets = presetsForCodec(this.presetsByCodec, this.codec)
      this.preset = defaultPresetName(this.presets)
    },

    get presetLabel(): string {
      return this.presets.find((p) => p.name === this.preset)?.label ?? this.preset
    },
    // Whether this album already has a queued/running job, read from the shared
    // `jobs` store the queue panel keeps in sync.
    get activeInQueue(): boolean {
      return stores(this)?.jobs?.isActive(this.path) ?? false
    },

    // The per-track selection lives in the `selection` store, shared with the
    // track table's checkbox column. Without it (no tracks rendered) the dock
    // falls back to "nothing selectable", which disables START.
    get selectedNames(): string[] {
      return stores(this)?.selection?.names ?? []
    },
    get selectedCount(): number {
      return this.selectedNames.length
    },
    get losslessCount(): number {
      return stores(this)?.selection?.losslessCount ?? 0
    },
    get lossyCount(): number {
      return Math.max(0, this.trackCount - this.losslessCount)
    },
    get hasLossy(): boolean {
      return this.lossyCount > 0
    },
    get skipLossy(): boolean {
      return stores(this)?.selection?.skipLossy ?? false
    },
    setSkipLossy(on: boolean) {
      stores(this)?.selection?.setSkipLossy(on)
    },
    get skippedCount(): number {
      return skippedCount(this.trackCount, this.selectedCount)
    },
    get showCopySkipped(): boolean {
      return this.skippedCount > 0 && this.outputMode !== 'replace'
    },
    // With nothing lossless in the directory no selection can lead to a job
    // START would accept, so the whole panel stays hidden rather than offering
    // two toggles that cannot change the "No lossless tracks" verdict.
    get showSelection(): boolean {
      return this.losslessCount > 0 && (this.hasLossy || this.showCopySkipped)
    },

    get startState(): StartState {
      return {
        canTranscode: this.canTranscode,
        trackCount: this.trackCount,
        selectedCount: this.selectedCount,
        starting: this.starting,
        outputMode: this.outputMode,
        activeInQueue: this.activeInQueue,
      }
    },
    get canStart(): boolean {
      return canStartTranscode(this.startState)
    },
    get statusText(): string {
      return startStatusText(this.startState)
    },

    setCodec(codec: Codec) {
      this.codec = codec
      this.presets = presetsForCodec(this.presetsByCodec, codec)
      this.preset = defaultPresetName(this.presets)
    },

    selectPreset(name: string) {
      this.preset = name
      this.presetOpen = false
    },

    async start() {
      if (!this.canStart) return
      if (
        this.outputMode === 'replace' &&
        !window.confirm('Replace mode will overwrite original files.\nA backup is made and restored on failure.\n\nContinue?')
      ) {
        return
      }

      this.starting = true
      this.error = ''
      this.resultText = ''
      const body = buildTranscodeRequestBody({
        path: this.path,
        codec: this.codec,
        preset: this.preset,
        outputMode: this.outputMode,
        skipLossy: this.skipLossy,
        files: this.selectedNames,
        trackCount: this.trackCount,
        copySkipped: this.copySkipped,
      })

      try {
        const res = await fetch('/api/transcode', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        const data: { job_id?: string; error?: string; code?: string } = await res.json().catch(() => ({}))
        if (!res.ok || !data.job_id) {
          // Every rejection now answers the {error, code} envelope, so the real
          // reason reaches the user. The generic text is not dead code: it still
          // covers a body that isn't JSON at all (proxy error page, truncated
          // response), where the .catch above yields {}.
          this.error = data.error || 'Failed to start transcoding'
          this.starting = false
          return
        }
        this.resultText = 'Queued — track progress in the queue below'
        this.resultOk = true
        this.starting = false
      } catch (err) {
        this.error = String(err)
        this.starting = false
      }
    },

    reindex() {
      const win = window as unknown as { altoTriggerScan?: (libraryId?: string) => void }
      if (typeof win.altoTriggerScan === 'function') {
        win.altoTriggerScan(this.libraryId)
      }
    },
  }
}
