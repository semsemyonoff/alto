import { beforeEach, describe, expect, it } from 'vitest'
import {
  altoDock,
  buildTranscodeRequestBody,
  canStartTranscode,
  defaultPresetName,
  parsePresets,
  presetsForCodec,
  readDockConfig,
  startStatusText,
  type PresetsByCodec,
} from './dock'

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
})

describe('canStartTranscode', () => {
  it('is true when transcodable, has tracks, a destination, not starting, not queued', () => {
    expect(canStartTranscode(true, 12, false, 'shared', false)).toBe(true)
  })

  it('is false until an output destination is chosen', () => {
    expect(canStartTranscode(true, 12, false, '', false)).toBe(false)
  })

  it('is false when this album already has an active job', () => {
    expect(canStartTranscode(true, 12, false, 'shared', true)).toBe(false)
  })

  it('is false when not transcodable (lossy source)', () => {
    expect(canStartTranscode(false, 12, false, 'shared', false)).toBe(false)
  })

  it('is false with zero tracks', () => {
    expect(canStartTranscode(true, 0, false, 'shared', false)).toBe(false)
  })

  it('is false while already starting', () => {
    expect(canStartTranscode(true, 12, true, 'shared', false)).toBe(false)
  })
})

describe('startStatusText', () => {
  it('reports that the album is already queued', () => {
    expect(startStatusText(true, 12, false, 'shared', true)).toBe('Already in the queue')
  })

  it('prompts to choose a destination when none is selected', () => {
    expect(startStatusText(true, 12, false, '', false)).toBe('Choose an output destination')
  })

  it('reports the disabled reason for a lossy directory', () => {
    expect(startStatusText(false, 12, false, '', false)).toBe('Lossless-only — this directory has lossy tracks')
  })

  it('reports starting while a request is in flight', () => {
    expect(startStatusText(true, 12, true, 'shared', false)).toBe('Starting…')
  })

  it('reports the track count when ready', () => {
    expect(startStatusText(true, 12, false, 'shared', false)).toBe('12 tracks')
  })

  it('uses singular phrasing for one track', () => {
    expect(startStatusText(true, 1, false, 'local', false)).toBe('1 track')
  })

  it('reports no tracks when the directory is empty', () => {
    expect(startStatusText(true, 0, false, 'shared', false)).toBe('No tracks to transcode')
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

describe('altoDock', () => {
  beforeEach(() => {
    document.body.innerHTML = `
      <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
      <aside id="tc-dock" data-path="/music/Jazz" data-can-transcode="true" data-track-count="7"></aside>
    `
  })

  function initDock() {
    const el = document.getElementById('tc-dock') as HTMLElement
    const dock = altoDock() as unknown as { $el: HTMLElement } & ReturnType<typeof altoDock>
    dock.$el = el
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

  it('disables start with a reason when the directory is not transcodable', () => {
    document.body.innerHTML = `
      <script type="application/json" id="tc-presets-data">${JSON.stringify(PRESETS)}</script>
      <aside id="tc-dock" data-path="/music/Lossy" data-can-transcode="false" data-track-count="3"></aside>
    `
    const dock = initDock()
    expect(dock.canStart).toBe(false)
    expect(dock.statusText).toBe('Lossless-only — this directory has lossy tracks')
  })
})
