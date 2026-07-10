import { describe, expect, it } from 'vitest'
import {
  activeDirs,
  bubbleText,
  cancelURL,
  deriveCounts,
  isCancelable,
  isRemovable,
  isScrolledToBottom,
  logURL,
  pctText,
  reconcileJob,
  removeURL,
  toRow,
  type JobEvent,
  type QueueRow,
} from './queue'

const JOB1: JobEvent = { id: 'job1', status: 'running', pct: 42, title: 'Album One', sub: 'FLAC → Opus 160k' }
const JOB2: JobEvent = { id: 'job2', status: 'queued', pct: 0, title: 'Album Two', sub: 'FLAC → Opus 160k' }

describe('toRow', () => {
  it('wraps a job event with a closed log expander', () => {
    expect(toRow(JOB1)).toEqual({ ...JOB1, logsOpen: false, logs: [], logsLoading: false, follow: true })
  })
})

describe('reconcileJob', () => {
  it('appends a new row for an unseen job id', () => {
    const rows = reconcileJob([], JOB1)
    expect(rows).toEqual([toRow(JOB1)])
  })

  it('appends a second unseen job after an existing one', () => {
    const rows = reconcileJob([toRow(JOB1)], JOB2)
    expect(rows.map((r) => r.id)).toEqual(['job1', 'job2'])
  })

  it('updates an existing row in place (server fields only)', () => {
    const rows = reconcileJob([toRow(JOB1)], { ...JOB1, status: 'done', pct: 100 })
    expect(rows).toHaveLength(1)
    expect(rows[0].status).toBe('done')
    expect(rows[0].pct).toBe(100)
  })

  it('preserves UI-only state (logsOpen/logs) across an update', () => {
    const opened: QueueRow = { ...toRow(JOB1), logsOpen: true, logs: ['line 1'] }
    const rows = reconcileJob([opened], { ...JOB1, status: 'done', pct: 100 })
    expect(rows[0].logsOpen).toBe(true)
    expect(rows[0].logs).toEqual(['line 1'])
  })

  it('reflects a cancel event (queued -> canceled)', () => {
    const rows = reconcileJob([toRow(JOB2)], { ...JOB2, status: 'canceled' })
    expect(rows[0].status).toBe('canceled')
  })
})

describe('deriveCounts', () => {
  it('counts running jobs as active and queued jobs as queued', () => {
    expect(deriveCounts([{ status: 'running' }, { status: 'queued' }, { status: 'queued' }, { status: 'done' }])).toEqual({
      active: 1,
      queued: 2,
    })
  })

  it('is zero/zero for an empty list', () => {
    expect(deriveCounts([])).toEqual({ active: 0, queued: 0 })
  })
})

describe('bubbleText', () => {
  it('reports idle for an empty queue', () => {
    expect(bubbleText([])).toBe('idle')
  })

  it('reports the active/queued breakdown for a non-empty queue', () => {
    expect(bubbleText([{ status: 'running' }, { status: 'queued' }])).toBe('1 active · 1 queued')
  })

  it('still reports a breakdown when nothing is active or queued (all terminal)', () => {
    expect(bubbleText([{ status: 'done' }, { status: 'failed' }])).toBe('0 active · 0 queued')
  })
})

describe('pctText', () => {
  it('shows an em dash for a queued job', () => {
    expect(pctText({ status: 'queued', pct: 0 })).toBe('—')
  })

  it('shows a rounded percentage for a running job', () => {
    expect(pctText({ status: 'running', pct: 61.7 })).toBe('62%')
  })

  it('shows 100% for a done job', () => {
    expect(pctText({ status: 'done', pct: 100 })).toBe('100%')
  })
})

describe('isCancelable', () => {
  it('is true for queued and running', () => {
    expect(isCancelable('queued')).toBe(true)
    expect(isCancelable('running')).toBe(true)
  })

  it('is false for terminal statuses', () => {
    expect(isCancelable('done')).toBe(false)
    expect(isCancelable('failed')).toBe(false)
    expect(isCancelable('canceled')).toBe(false)
  })
})

describe('isRemovable', () => {
  it('is true for terminal statuses', () => {
    expect(isRemovable('done')).toBe(true)
    expect(isRemovable('failed')).toBe(true)
    expect(isRemovable('canceled')).toBe(true)
  })

  it('is false while queued or running', () => {
    expect(isRemovable('queued')).toBe(false)
    expect(isRemovable('running')).toBe(false)
  })
})

describe('cancelURL', () => {
  it('builds the POST /api/jobs/{id}/cancel URL', () => {
    expect(cancelURL('job1')).toBe('/api/jobs/job1/cancel')
  })

  it('encodes ids that need escaping', () => {
    expect(cancelURL('a/b')).toBe('/api/jobs/a%2Fb/cancel')
  })
})

describe('removeURL', () => {
  it('builds the POST /api/jobs/{id}/remove URL', () => {
    expect(removeURL('job1')).toBe('/api/jobs/job1/remove')
  })

  it('encodes ids that need escaping', () => {
    expect(removeURL('a/b')).toBe('/api/jobs/a%2Fb/remove')
  })
})

describe('activeDirs', () => {
  it('lists source dirs of queued/running jobs only', () => {
    expect(
      activeDirs([
        { status: 'running', dir: '/music/A' },
        { status: 'queued', dir: '/music/B' },
        { status: 'done', dir: '/music/C' },
        { status: 'failed', dir: '/music/D' },
      ]),
    ).toEqual(['/music/A', '/music/B'])
  })

  it('skips active jobs with no dir', () => {
    expect(activeDirs([{ status: 'running' }, { status: 'queued', dir: '/music/B' }])).toEqual(['/music/B'])
  })
})

describe('isScrolledToBottom', () => {
  it('is true at the bottom (within the tolerance)', () => {
    expect(isScrolledToBottom({ scrollHeight: 500, scrollTop: 380, clientHeight: 120 })).toBe(true)
  })

  it('is false when scrolled up', () => {
    expect(isScrolledToBottom({ scrollHeight: 500, scrollTop: 100, clientHeight: 120 })).toBe(false)
  })
})

describe('logURL', () => {
  it('builds the GET /api/transcode/{id}/log URL', () => {
    expect(logURL('job1')).toBe('/api/transcode/job1/log')
  })
})
