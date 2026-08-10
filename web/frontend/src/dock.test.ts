import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  altoDock,
  buildTranscodeRequestBody,
  canStartTranscode,
  defaultPresetName,
  parsePresets,
  presetsForCodec,
  readDockConfig,
  skippedCount,
  startStatusText,
  type PresetsByCodec,
  type StartState,
} from './dock'
import { selectionStore, type SelectionStore, type TrackInfo } from './selection'

const PRESETS: PresetsByCodec = {
  flac: [
    { name: 'Fast', label: 'Fast (compression 0)' },
    { name: 'Balanced', label: 'Balanced (compression 5)', default: true },
    { name: 'Max Compression', label: 'Max Compression (compression 8)' },
  ],
  opus: [
    { name: 'Music Balanced', label: 'Music Balanced (128k)' },
    { name: 'Music High', label: 'Music High (160k)', default: true },
    { name: 'Archive Lossy', label: 'Archive Lossy (192k)' },
  ],
}

describe('presetsForCodec', () => {
  it('returns the presets for a known codec', () => {
    expect(presetsForCodec(PRESETS, 'opus')).toEqual(PRESETS.opus)
  })

  it('returns an empty list for an unknown codec', () => {
    expect(presetsForCodec(PRESETS, 'mp3')).toEqual([])
  })
})

describe('defaultPresetName', () => {
  it('returns the preset flagged default', () => {
    expect(defaultPresetName(PRESETS.opus)).toBe('Music High')
  })

  it('falls back to the first preset when none is flagged default', () => {
    expect(defaultPresetName([{ name: 'Only', label: 'Only preset' }])).toBe('Only')
  })

  it('returns an empty string for an empty list', () => {
    expect(defaultPresetName([])).toBe('')
  })
})

describe('parsePresets', () => {
  it('parses valid JSON', () => {
    expect(parsePresets(JSON.stringify(PRESETS))).toEqual(PRESETS)
  })

  it('returns an empty object for null/undefined/empty input', () => {
    expect(parsePresets(null)).toEqual({})
    expect(parsePresets(undefined)).toEqual({})
    expect(parsePresets('')).toEqual({})
  })

  it('returns an empty object for invalid JSON', () => {
    expect(parsePresets('{not json')).toEqual({})
  })

  it('returns an empty object for a non-object JSON value', () => {
    expect(parsePresets('42')).toEqual({})
  })
})

describe('buildTranscodeRequestBody', () => {
  it('builds a request body matching the server transcodeRequest shape', () => {
    expect(
      buildTranscodeRequestBody({ path: '/music/Jazz', codec: 'flac', preset: 'Balanced', outputMode: 'shared' }),
    ).toEqual({ path: '/music/Jazz', codec: 'flac', preset: 'Balanced', output_mode: 'shared' })
  })

  it('carries the replace output mode through untouched', () => {
    expect(
      buildTranscodeRequestBody({ path: '/music/X', codec: 'opus', preset: 'Music High', outputMode: 'replace' }),
    ).toEqual({ path: '/music/X', codec: 'opus', preset: 'Music High', output_mode: 'replace' })
  })

  it('omits both selection fields when every track is selected', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Jazz',
      codec: 'flac',
      preset: 'Balanced',
      outputMode: 'shared',
      files: ['01 A.flac', '02 B.flac'],
      trackCount: 2,
    })
    expect(body).toEqual({ path: '/music/Jazz', codec: 'flac', preset: 'Balanced', output_mode: 'shared' })
  })

  it('sends skip_lossy alone when the toggle is on', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Mixed',
      codec: 'opus',
      preset: 'Music High',
      outputMode: 'shared',
      skipLossy: true,
      files: ['01 A.flac'],
      trackCount: 3,
    })
    expect(body.skip_lossy).toBe(true)
    expect(body.files).toBeUndefined()
  })

  it('sends files alone for a manual subset', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Mixed',
      codec: 'opus',
      preset: 'Music High',
      outputMode: 'shared',
      files: ['01 A.flac', '03 C.flac'],
      trackCount: 4,
    })
    expect(body.files).toEqual(['01 A.flac', '03 C.flac'])
    expect(body.skip_lossy).toBeUndefined()
  })

  it('never sends an empty files list', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Mixed',
      codec: 'opus',
      preset: 'Music High',
      outputMode: 'shared',
      files: [],
      trackCount: 3,
    })
    expect(body.files).toBeUndefined()
    expect(body.skip_lossy).toBeUndefined()
  })

  it('sends copy_skipped when something is skipped', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Mixed',
      codec: 'opus',
      preset: 'Music High',
      outputMode: 'shared',
      skipLossy: true,
      files: ['01 A.flac'],
      trackCount: 3,
      copySkipped: true,
    })
    expect(body.copy_skipped).toBe(true)
  })

  it('omits copy_skipped in replace mode — the server rejects the combination', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Mixed',
      codec: 'flac',
      preset: 'Balanced',
      outputMode: 'replace',
      skipLossy: true,
      files: ['01 A.flac'],
      trackCount: 3,
      copySkipped: true,
    })
    expect(body.copy_skipped).toBeUndefined()
  })

  it('omits copy_skipped when nothing is skipped', () => {
    const body = buildTranscodeRequestBody({
      path: '/music/Jazz',
      codec: 'flac',
      preset: 'Balanced',
      outputMode: 'shared',
      files: ['01 A.flac', '02 B.flac'],
      trackCount: 2,
      copySkipped: true,
    })
    expect(body.copy_skipped).toBeUndefined()
  })
})

describe('skippedCount', () => {
  it('counts the tracks the selection leaves out', () => {
    expect(skippedCount(10, 8)).toBe(2)
  })

  it('never goes negative', () => {
    expect(skippedCount(2, 5)).toBe(0)
  })
})

/** A ready-to-start state; each test overrides only the field it exercises. */
function state(overrides: Partial<StartState> = {}): StartState {
  return {
    canTranscode: true,
    trackCount: 12,
    selectedCount: 12,
    starting: false,
    outputMode: 'shared',
    activeInQueue: false,
    ...overrides,
  }
}

describe('canStartTranscode', () => {
  it('is true when transcodable, has a selection, a destination, not starting, not queued', () => {
    expect(canStartTranscode(state())).toBe(true)
  })

  it('is false until an output destination is chosen', () => {
    expect(canStartTranscode(state({ outputMode: '' }))).toBe(false)
  })

  it('is false when this album already has an active job', () => {
    expect(canStartTranscode(state({ activeInQueue: true }))).toBe(false)
  })

  it('is false when no track is lossless', () => {
    expect(canStartTranscode(state({ canTranscode: false, selectedCount: 0 }))).toBe(false)
  })

  it('is false with zero tracks', () => {
    expect(canStartTranscode(state({ trackCount: 0, selectedCount: 0 }))).toBe(false)
  })

  it('is false when the user deselected every track', () => {
    expect(canStartTranscode(state({ selectedCount: 0 }))).toBe(false)
  })

  it('is true for a partial selection', () => {
    expect(canStartTranscode(state({ selectedCount: 3 }))).toBe(true)
  })

  it('is false while already starting', () => {
    expect(canStartTranscode(state({ starting: true }))).toBe(false)
  })
})

describe('startStatusText', () => {
  it('reports that the album is already queued', () => {
    expect(startStatusText(state({ activeInQueue: true }))).toBe('Already in the queue')
  })

  it('prompts to choose a destination when none is selected', () => {
    expect(startStatusText(state({ outputMode: '' }))).toBe('Choose an output destination')
  })

  it('reports the disabled reason for an all-lossy directory', () => {
    expect(startStatusText(state({ canTranscode: false, selectedCount: 0, outputMode: '' }))).toBe('No lossless tracks')
  })

  it('reports an empty selection', () => {
    expect(startStatusText(state({ selectedCount: 0 }))).toBe('Nothing selected')
  })

  it('reports starting while a request is in flight', () => {
    expect(startStatusText(state({ starting: true }))).toBe('Starting…')
  })

  it('reports the track count when everything is selected', () => {
    expect(startStatusText(state())).toBe('12 tracks')
  })

  it('reports the subset size when only some tracks are selected', () => {
    expect(startStatusText(state({ selectedCount: 2, trackCount: 3 }))).toBe('2 of 3 tracks')
  })

  it('uses singular phrasing for one track', () => {
    expect(startStatusText(state({ trackCount: 1, selectedCount: 1, outputMode: 'local' }))).toBe('1 track')
  })

  it('reports no tracks when the directory is empty', () => {
    expect(startStatusText(state({ trackCount: 0, selectedCount: 0 }))).toBe('No tracks to transcode')
  })
})

describe('readDockConfig', () => {
  beforeEach(() => {
    document.body.innerHTML = ''
  })

  it('reads path/eligibility/track count/library id from data attributes and presets from the JSON script tag', () => {
    document.body.innerHTML = `
      <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
      <aside id="tc-dock" data-path="/music/Jazz" data-can-transcode="true" data-track-count="7" data-library-id="3"></aside>
    `
    const el = document.getElementById('tc-dock') as HTMLElement
    expect(readDockConfig(el)).toEqual({
      path: '/music/Jazz',
      canTranscode: true,
      trackCount: 7,
      libraryId: '3',
      presets: PRESETS,
    })
  })

  it('defaults canTranscode to false and presets to empty when attributes/script are missing', () => {
    document.body.innerHTML = `<aside id="tc-dock" data-path="/music/X"></aside>`
    const el = document.getElementById('tc-dock') as HTMLElement
    expect(readDockConfig(el)).toEqual({
      path: '/music/X',
      canTranscode: false,
      trackCount: 0,
      libraryId: '',
      presets: {},
    })
  })
})

/** n lossless FLAC tracks, matching what the page's tc-tracks-data would carry. */
function flacTracks(n: number): TrackInfo[] {
  return Array.from({ length: n }, (_, i) => ({ name: `0${i + 1} A.flac`, codec: 'flac', lossless: true }))
}

describe('altoDock', () => {
  beforeEach(() => {
    document.body.innerHTML = `
      <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
      <aside id="tc-dock" data-path="/music/Jazz" data-can-transcode="true" data-track-count="7"></aside>
    `
  })

  type TestDock = { $el: HTMLElement; $store: { selection: SelectionStore } } & ReturnType<typeof altoDock>

  // The dock reads the per-track selection from the shared `selection` store, so
  // the tests wire up the real store rather than a stand-in.
  function initDock(tracks: TrackInfo[] = flacTracks(7)): TestDock {
    const el = document.getElementById('tc-dock') as HTMLElement
    const store = selectionStore()
    store.init(el.dataset.path ?? '', tracks)
    const dock = altoDock() as unknown as TestDock
    dock.$el = el
    dock.$store = { selection: store }
    dock.init()
    return dock
  }

  it('initializes with the FLAC codec and its default preset, but no output destination', () => {
    const dock = initDock()
    expect(dock.codec).toBe('flac')
    expect(dock.preset).toBe('Balanced')
    expect(dock.presetLabel).toBe('Balanced (compression 5)')
    // No destination chosen yet: START stays disabled until the user picks one.
    expect(dock.outputMode).toBe('')
    expect(dock.canStart).toBe(false)
    expect(dock.statusText).toBe('Choose an output destination')
  })

  it('enables START once an output destination is chosen', () => {
    const dock = initDock()
    dock.outputMode = 'shared'
    expect(dock.canStart).toBe(true)
    expect(dock.statusText).toBe('7 tracks')
  })

  it('switches presets when the codec changes', () => {
    const dock = initDock()
    dock.setCodec('opus')
    expect(dock.codec).toBe('opus')
    expect(dock.preset).toBe('Music High')
    expect(dock.presets).toEqual(PRESETS.opus)
  })

  it('selects a preset by name and closes the dropdown', () => {
    const dock = initDock()
    dock.presetOpen = true
    dock.selectPreset('Fast')
    expect(dock.preset).toBe('Fast')
    expect(dock.presetOpen).toBe(false)
  })

  it('delegates reindex() to window.altoTriggerScan with the dock library id', () => {
    document.body.innerHTML = `
      <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
      <aside id="tc-dock" data-path="/music/Jazz" data-can-transcode="true" data-track-count="7" data-library-id="5"></aside>
    `
    const calls: Array<string | undefined> = []
    ;(window as unknown as { altoTriggerScan?: (libraryId?: string) => void }).altoTriggerScan = (libraryId) => {
      calls.push(libraryId)
    }
    const dock = initDock()
    dock.reindex()
    expect(calls).toEqual(['5'])
  })

  it('disables start with a reason when no track is lossless', () => {
    document.body.innerHTML = `
      <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
      <aside id="tc-dock" data-path="/music/Lossy" data-can-transcode="false" data-track-count="3"></aside>
    `
    const dock = initDock([
      { name: '01 A.mp3', codec: 'mp3', lossless: false },
      { name: '02 B.mp3', codec: 'mp3', lossless: false },
      { name: '03 C.mp3', codec: 'mp3', lossless: false },
    ])
    expect(dock.canStart).toBe(false)
    expect(dock.statusText).toBe('No lossless tracks')
  })

  describe('selection', () => {
    const MIXED: TrackInfo[] = [
      { name: '01 A.flac', codec: 'flac', lossless: true },
      { name: '02 B.flac', codec: 'flac', lossless: true },
      { name: '03 C.mp3', codec: 'mp3', lossless: false },
    ]

    function initMixedDock(): TestDock {
      document.body.innerHTML = `
        <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
        <aside id="tc-dock" data-path="/music/Mixed" data-can-transcode="true" data-track-count="3"></aside>
      `
      const dock = initDock(MIXED)
      dock.outputMode = 'shared'
      return dock
    }

    it('offers the skip-lossy toggle on a mixed directory, defaulted on', () => {
      const dock = initMixedDock()
      expect(dock.hasLossy).toBe(true)
      expect(dock.lossyCount).toBe(1)
      expect(dock.skipLossy).toBe(true)
      expect(dock.statusText).toBe('2 of 3 tracks')
      expect(dock.canStart).toBe(true)
    })

    it('does not offer the toggle on an all-lossless directory', () => {
      const dock = initDock()
      expect(dock.hasLossy).toBe(false)
      expect(dock.lossyCount).toBe(0)
      expect(dock.skipLossy).toBe(false)
    })

    it('offers the copy-skipped checkbox only outside replace mode', () => {
      const dock = initMixedDock()
      expect(dock.skippedCount).toBe(1)
      expect(dock.showCopySkipped).toBe(true)
      dock.outputMode = 'replace'
      expect(dock.showCopySkipped).toBe(false)
    })

    it('does not offer the copy-skipped checkbox when nothing is skipped', () => {
      const dock = initDock()
      dock.outputMode = 'shared'
      expect(dock.skippedCount).toBe(0)
      expect(dock.showCopySkipped).toBe(false)
    })

    it('clears the toggle when a row checkbox is touched', () => {
      const dock = initMixedDock()
      dock.$store.selection.toggle('02 B.flac')
      expect(dock.skipLossy).toBe(false)
      expect(dock.selectedNames).toEqual(['01 A.flac'])
      expect(dock.statusText).toBe('1 of 3 tracks')
    })

    it('restores the full lossless selection when the toggle is turned back on', () => {
      const dock = initMixedDock()
      dock.$store.selection.toggle('02 B.flac')
      dock.setSkipLossy(true)
      expect(dock.skipLossy).toBe(true)
      expect(dock.selectedNames).toEqual(['01 A.flac', '02 B.flac'])
    })

    it('reports nothing selected once every track is deselected', () => {
      const dock = initMixedDock()
      dock.$store.selection.toggleAll()
      expect(dock.selectedCount).toBe(0)
      expect(dock.statusText).toBe('Nothing selected')
      expect(dock.canStart).toBe(false)
    })
  })

  describe('start', () => {
    function stubFetch(response: { ok: boolean; body: unknown } | { ok: boolean; text: string }) {
      const calls: Array<{ url: string; body: unknown }> = []
      vi.stubGlobal('fetch', (url: string, init: RequestInit) => {
        calls.push({ url, body: JSON.parse(String(init.body)) })
        return Promise.resolve({
          ok: response.ok,
          json: () =>
            'body' in response ? Promise.resolve(response.body) : Promise.reject(new Error('not JSON')),
        } as Response)
      })
      return calls
    }

    afterEach(() => {
      vi.unstubAllGlobals()
    })

    it('posts skip_lossy and copy_skipped for a mixed directory', async () => {
      document.body.innerHTML = `
        <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
        <aside id="tc-dock" data-path="/music/Mixed" data-can-transcode="true" data-track-count="3"></aside>
      `
      const dock = initDock([
        { name: '01 A.flac', codec: 'flac', lossless: true },
        { name: '02 B.mp3', codec: 'mp3', lossless: false },
        { name: '03 C.mp3', codec: 'mp3', lossless: false },
      ])
      dock.outputMode = 'shared'
      dock.copySkipped = true
      const calls = stubFetch({ ok: true, body: { job_id: 'abc123' } })

      await dock.start()

      expect(calls).toHaveLength(1)
      expect(calls[0].url).toBe('/api/transcode')
      expect(calls[0].body).toEqual({
        path: '/music/Mixed',
        codec: 'flac',
        preset: 'Balanced',
        output_mode: 'shared',
        skip_lossy: true,
        copy_skipped: true,
      })
      expect(dock.resultOk).toBe(true)
      expect(dock.error).toBe('')
    })

    it('surfaces the server error message from the JSON envelope', async () => {
      const dock = initDock()
      dock.outputMode = 'shared'
      stubFetch({ ok: false, body: { error: 'directory has no lossless tracks', code: 'no_lossless_tracks' } })

      await dock.start()

      expect(dock.error).toBe('directory has no lossless tracks')
      expect(dock.starting).toBe(false)
    })

    it('falls back to a generic message when the response is not JSON', async () => {
      const dock = initDock()
      dock.outputMode = 'shared'
      stubFetch({ ok: false, text: 'Bad Gateway' })

      await dock.start()

      expect(dock.error).toBe('Failed to start transcoding')
    })
  })
})
