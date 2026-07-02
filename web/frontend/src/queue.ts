/**
 * The global transcode queue panel: an Alpine island rendered in the app
 * shell (base.html), outside the HTMX swap targets, so it persists across
 * index<->directory navigation. It loads the current job list from
 * GET /api/jobs, then reconciles live `update` events from the
 * GET /api/jobs/events SSE stream. Row click lazily loads
 * GET /api/transcode/{id}/log; the "x" cancels via
 * POST /api/jobs/{id}/cancel.
 */

export type JobStatus = 'queued' | 'running' | 'done' | 'failed' | 'canceled'

/** The shape of a GET /api/jobs entry and a GET /api/jobs/events `update` payload. */
export interface JobEvent {
  id: string
  status: JobStatus
  pct: number
  title: string
  sub: string
}

/** A job row plus UI-only state for the queue panel's log expander. */
export interface QueueRow extends JobEvent {
  logsOpen: boolean
  logs: string[]
  logsLoading: boolean
}

/** Wraps a server job event into a fresh queue row with its log expander closed. */
export function toRow(event: JobEvent): QueueRow {
  return { ...event, logsOpen: false, logs: [], logsLoading: false }
}

/**
 * Reconciles a single `update` event into the row list: updates the server
 * fields of an existing row in place (preserving its logsOpen/logs UI
 * state), or appends a new row when the event names an id not yet seen.
 */
export function reconcileJob(rows: QueueRow[], event: JobEvent): QueueRow[] {
  const i = rows.findIndex((r) => r.id === event.id)
  if (i === -1) return [...rows, toRow(event)]
  const next = rows.slice()
  next[i] = { ...next[i], status: event.status, pct: event.pct, title: event.title, sub: event.sub }
  return next
}

export interface QueueCounts {
  active: number
  queued: number
}

/** Derives the queue bubble's active/queued counts. */
export function deriveCounts(jobs: { status: JobStatus }[]): QueueCounts {
  return {
    active: jobs.filter((j) => j.status === 'running').length,
    queued: jobs.filter((j) => j.status === 'queued').length,
  }
}

/** The text shown in the queue-head bubble: "idle" when empty, else the active/queued breakdown. */
export function bubbleText(jobs: { status: JobStatus }[]): string {
  if (jobs.length === 0) return 'idle'
  const { active, queued } = deriveCounts(jobs)
  return `${active} active · ${queued} queued`
}

/** The row's percent readout: an em dash while queued (no progress yet), else a rounded percentage. */
export function pctText(job: { status: JobStatus; pct: number }): string {
  return job.status === 'queued' ? '—' : `${Math.round(job.pct)}%`
}

/** Reports whether a job can still be canceled (queued or running). */
export function isCancelable(status: JobStatus): boolean {
  return status === 'queued' || status === 'running'
}

export function cancelURL(id: string): string {
  return `/api/jobs/${encodeURIComponent(id)}/cancel`
}

export function logURL(id: string): string {
  return `/api/transcode/${encodeURIComponent(id)}/log`
}

interface QueueData {
  jobs: QueueRow[]
  collapsed: boolean
  _es: EventSource | null
  init(): Promise<void>
  connect(): void
  readonly bubble: string
  pctText(job: QueueRow): string
  isCancelable(job: QueueRow): boolean
  toggleCollapse(): void
  toggleLogs(job: QueueRow): void
  fetchLogs(job: QueueRow): Promise<void>
  cancel(job: QueueRow): Promise<void>
}

/** Alpine.data factory registered as `altoQueue` and referenced via `x-data="altoQueue()"`. */
export function altoQueue(): QueueData {
  return {
    jobs: [],
    collapsed: false,
    _es: null,

    async init() {
      try {
        const res = await fetch('/api/jobs')
        const data: { jobs?: JobEvent[] } = await res.json().catch(() => ({}))
        this.jobs = (data.jobs ?? []).map(toRow)
      } catch {
        this.jobs = []
      }
      this.connect()
    },

    connect() {
      this._es?.close()
      const es = new EventSource('/api/jobs/events')
      this._es = es
      es.addEventListener('update', (e) => {
        try {
          const event: JobEvent = JSON.parse((e as MessageEvent).data)
          this.jobs = reconcileJob(this.jobs, event)
        } catch {
          // ignore malformed events
        }
      })
    },

    get bubble(): string {
      return bubbleText(this.jobs)
    },

    pctText(job: QueueRow): string {
      return pctText(job)
    },

    isCancelable(job: QueueRow): boolean {
      return isCancelable(job.status)
    },

    toggleCollapse() {
      this.collapsed = !this.collapsed
    },

    toggleLogs(job: QueueRow) {
      job.logsOpen = !job.logsOpen
      if (job.logsOpen && job.logs.length === 0 && !job.logsLoading) {
        void this.fetchLogs(job)
      }
    },

    async fetchLogs(job: QueueRow) {
      job.logsLoading = true
      try {
        const res = await fetch(logURL(job.id))
        const data: { lines?: string[] } = await res.json().catch(() => ({}))
        job.logs = data.lines ?? []
      } catch {
        job.logs = []
      } finally {
        job.logsLoading = false
      }
    },

    async cancel(job: QueueRow) {
      try {
        await fetch(cancelURL(job.id), { method: 'POST' })
      } catch {
        // ignore; the SSE stream reconciles the actual state
      }
    },
  }
}
