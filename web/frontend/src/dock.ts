/**
 * The transcode dock: an Alpine island rendered beside a directory's track
 * table. It filters transcode.DefaultPresets() (marshaled server-side into a
 * per-page <script id="tc-presets-data"> tag) by the selected codec, builds
 * the POST /api/transcode request body, and disables START with a reason
 * when the directory isn't transcodable.
 */

export type Codec = 'flac' | 'opus'
export type OutputMode = 'shared' | 'local' | 'replace'

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
}

export interface TranscodeRequestBody {
  path: string
  codec: string
  preset: string
  output_mode: string
}

/** Builds the JSON body for POST /api/transcode, matching the server's transcodeRequest shape. */
export function buildTranscodeRequestBody(input: TranscodeRequestInput): TranscodeRequestBody {
  return {
    path: input.path,
    codec: input.codec,
    preset: input.preset,
    output_mode: input.outputMode,
  }
}

/** Reports whether the dock's START control should be enabled. */
export function canStartTranscode(canTranscode: boolean, trackCount: number, starting: boolean): boolean {
  return canTranscode && trackCount > 0 && !starting
}

/** The status line shown under START — a disabled reason, or the track count when ready. */
export function startStatusText(canTranscode: boolean, trackCount: number, starting: boolean): string {
  if (starting) return 'Starting…'
  if (trackCount <= 0) return 'No tracks to transcode'
  if (!canTranscode) return 'Lossless-only — this directory has lossy tracks'
  return `${trackCount} track${trackCount === 1 ? '' : 's'}`
}

export interface ProgressReport {
  current_file?: string
  overall_percent?: number
}

/** Derives the dock's progress readout from a `progress` SSE event payload. */
export function progressFileText(report: ProgressReport): string {
  return report.current_file || 'Processing…'
}

export interface DoneReport {
  status?: string
  error?: string
}

export interface DoneResult {
  text: string
  ok: boolean
}

/** Derives the dock's result line from a `done` SSE event payload. */
export function doneResult(report: DoneReport): DoneResult {
  if (report.status === 'done') {
    return { text: '✓ Transcoding complete', ok: true }
  }
  return { text: `✗ ${report.error || 'Transcoding failed'}`, ok: false }
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
  starting: boolean
  error: string
  path: string
  canTranscode: boolean
  trackCount: number
  libraryId: string
  jobId: string | null
  progressPct: number
  progressFile: string
  resultText: string
  resultOk: boolean
  _es: EventSource | null
  init(): void
  readonly presetLabel: string
  readonly canStart: boolean
  readonly statusText: string
  setCodec(codec: Codec): void
  selectPreset(name: string): void
  start(): Promise<void>
  watchProgress(jobId: string): void
  reindex(): void
}

/** Alpine.data factory registered as `altoDock` and referenced via `x-data="altoDock()"`. */
export function altoDock(): DockData {
  return {
    codec: 'flac',
    presetsByCodec: {},
    presets: [],
    preset: '',
    presetOpen: false,
    outputMode: 'shared',
    starting: false,
    error: '',
    path: '',
    canTranscode: false,
    trackCount: 0,
    libraryId: '',
    jobId: null,
    progressPct: 0,
    progressFile: '',
    resultText: '',
    resultOk: false,
    _es: null,

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
    get canStart(): boolean {
      return canStartTranscode(this.canTranscode, this.trackCount, this.starting)
    },
    get statusText(): string {
      return startStatusText(this.canTranscode, this.trackCount, this.starting)
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
      })

      try {
        const res = await fetch('/api/transcode', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        })
        const data: { job_id?: string; error?: string } = await res.json().catch(() => ({}))
        if (!res.ok || !data.job_id) {
          this.error = data.error || 'Failed to start transcoding'
          this.starting = false
          return
        }
        this.jobId = data.job_id
        this.watchProgress(data.job_id)
      } catch (err) {
        this.error = String(err)
        this.starting = false
      }
    },

    watchProgress(jobId: string) {
      this._es?.close()
      const es = new EventSource(`/api/transcode/${jobId}/progress`)
      this._es = es

      es.addEventListener('progress', (e) => {
        try {
          const report: ProgressReport = JSON.parse((e as MessageEvent).data)
          this.progressFile = progressFileText(report)
          this.progressPct = report.overall_percent || 0
        } catch {
          // ignore malformed events
        }
      })

      es.addEventListener('done', (e) => {
        es.close()
        this._es = null
        this.starting = false
        try {
          const report: DoneReport = JSON.parse((e as MessageEvent).data)
          const result = doneResult(report)
          this.resultText = result.text
          this.resultOk = result.ok
          if (result.ok) this.progressPct = 100
        } catch {
          // ignore malformed events
        }
      })

      es.onerror = () => {
        if (!this.resultText) {
          this.resultText = '✗ Connection lost — check server log'
          this.resultOk = false
        }
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
