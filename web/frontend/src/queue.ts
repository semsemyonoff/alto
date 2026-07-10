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
  /** Absolute source directory; lets the dock detect an active job for its album. */
  dir?: string
}

/** A job row plus UI-only state for the queue panel's log expander. */
export interface QueueRow extends JobEvent {
  logsOpen: boolean
  logs: string[]
  logsLoading: boolean
  /** When the log panel is open, auto-scroll to the newest line (tail -f). */
  follow: boolean
}

/** Wraps a server job event into a fresh queue row with its log expander closed. */
export function toRow(event: JobEvent): QueueRow {
  return { ...event, logsOpen: false, logs: [], logsLoading: false, follow: true }
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

/** Reports whether a job has reached a terminal state and can be dismissed from the queue. */
export function isRemovable(status: JobStatus): boolean {
  return !isCancelable(status)
}

/** The absolute source dirs of every active (queued/running) job — the dock's START gate. */
export function activeDirs(jobs: { status: JobStatus; dir?: string }[]): string[] {
  return jobs.filter((j) => isCancelable(j.status) && j.dir).map((j) => j.dir as string)
}

/** Whether the user is scrolled to (near) the bottom of a log container. */
export function isScrolledToBottom(el: { scrollHeight: number; scrollTop: number; clientHeight: number }): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight < 24
}

export function cancelURL(id: string): string {
  return `/api/jobs/${encodeURIComponent(id)}/cancel`
}

export function removeURL(id: string): string {
  return `/api/jobs/${encodeURIComponent(id)}/remove`
}

export function logURL(id: string): string {
  return `/api/transcode/${encodeURIComponent(id)}/log`
}

interface QueueData {
  jobs: QueueRow[]
  collapsed: boolean
  _es: EventSource | null
  _timers: Record<string, ReturnType<typeof setInterval>>
  init(): Promise<void>
  connect(): void
  syncActiveDirs(): void
  readonly bubble: string
  pctText(job: QueueRow): string
  isCancelable(job: QueueRow): boolean
  isRemovable(job: QueueRow): boolean
  toggleCollapse(): void
  toggleLogs(job: QueueRow): void
  fetchLogs(job: QueueRow): Promise<void>
  cancel(job: QueueRow): Promise<void>
  remove(job: QueueRow): Promise<void>
  toggleFollow(job: QueueRow): void
  onLogScroll(job: QueueRow, event: Event): void
  startLogPolling(job: QueueRow): void
  stopLogPolling(job: QueueRow): void
  scrollLogToBottom(job: QueueRow): void
}

/** Alpine.data factory registered as `altoQueue` and referenced via `x-data="altoQueue()"`. */
export function altoQueue(): QueueData {
  return {
    jobs: [],
    // Collapsed by default: the queue starts tucked away and the user expands it
    // when they want to watch progress.
    collapsed: true,
    _es: null,
    _timers: {},

    async init() {
      try {
        const res = await fetch('/api/jobs')
        const data: { jobs?: JobEvent[] } = await res.json().catch(() => ({}))
        this.jobs = (data.jobs ?? []).map(toRow)
      } catch {
        this.jobs = []
      }
      this.syncActiveDirs()
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
          this.syncActiveDirs()
        } catch {
          // ignore malformed events
        }
      })
      // A `remove` event fires when a job is dismissed from the queue (here or
      // in another open tab); drop the row and stop any log polling for it.
      es.addEventListener('remove', (e) => {
        try {
          const { id }: { id: string } = JSON.parse((e as MessageEvent).data)
          const row = this.jobs.find((j) => j.id === id)
          if (row) this.stopLogPolling(row)
          this.jobs = this.jobs.filter((j) => j.id !== id)
          this.syncActiveDirs()
        } catch {
          // ignore malformed events
        }
      })
    },

    // Publishes the set of active-job source dirs to the shared Alpine store so
    // the transcode dock can disable START for an album already in the queue.
    syncActiveDirs() {
      const store = (this as unknown as { $store?: { jobs?: { activeDirs: string[] } } }).$store
      if (store?.jobs) store.jobs.activeDirs = activeDirs(this.jobs)
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

    isRemovable(job: QueueRow): boolean {
      return isRemovable(job.status)
    },

    toggleCollapse() {
      this.collapsed = !this.collapsed
    },

    toggleLogs(job: QueueRow) {
      job.logsOpen = !job.logsOpen
      if (job.logsOpen) {
        if (job.logs.length === 0 && !job.logsLoading) {
          void this.fetchLogs(job)
        }
        // Keep an open log panel current for a still-active job (tail -f).
        if (isCancelable(job.status)) this.startLogPolling(job)
      } else {
        this.stopLogPolling(job)
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
        if (job.follow) this.scrollLogToBottom(job)
      }
    },

    // Polls the log endpoint once a second while an active job's panel is open,
    // stopping after the first refresh that observes the job as finished (so the
    // final lines land). Follow-scroll happens inside fetchLogs.
    startLogPolling(job: QueueRow) {
      this.stopLogPolling(job)
      this._timers[job.id] = setInterval(() => {
        if (!job.logsOpen) {
          this.stopLogPolling(job)
          return
        }
        void this.fetchLogs(job)
        if (!isCancelable(job.status)) this.stopLogPolling(job)
      }, 1000)
    },

    stopLogPolling(job: QueueRow) {
      const t = this._timers[job.id]
      if (t !== undefined) {
        clearInterval(t)
        delete this._timers[job.id]
      }
    },

    toggleFollow(job: QueueRow) {
      job.follow = !job.follow
      if (job.follow) this.scrollLogToBottom(job)
    },

    // Bound to the log container's scroll: scrolling up disengages follow so the
    // user can read freely; scrolling back to the bottom re-engages it. Because
    // programmatic scroll-to-bottom lands at the bottom, it never spuriously
    // disables follow.
    onLogScroll(job: QueueRow, event: Event) {
      job.follow = isScrolledToBottom(event.target as HTMLElement)
    },

    scrollLogToBottom(job: QueueRow) {
      // $nextTick is an Alpine magic injected at runtime (see dock.ts for the
      // same $el cast pattern); wait for the new log lines to render, then pin
      // the scroll to the bottom.
      const nextTick = (this as unknown as { $nextTick: (cb: () => void) => void }).$nextTick
      nextTick(() => {
        const el = document.querySelector<HTMLElement>(`[data-log-scroll="${job.id}"]`)
        if (el) el.scrollTop = el.scrollHeight
      })
    },

    async cancel(job: QueueRow) {
      try {
        await fetch(cancelURL(job.id), { method: 'POST' })
      } catch {
        // ignore; the SSE stream reconciles the actual state
      }
    },

    async remove(job: QueueRow) {
      this.stopLogPolling(job)
      // Optimistically drop the row; the `remove` SSE event reconciles other tabs.
      this.jobs = this.jobs.filter((j) => j.id !== job.id)
      this.syncActiveDirs()
      try {
        await fetch(removeURL(job.id), { method: 'POST' })
      } catch {
        // ignore; a reload re-fetches the authoritative list
      }
    },
  }
}
