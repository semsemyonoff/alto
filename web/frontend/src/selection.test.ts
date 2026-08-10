import { describe, expect, it } from 'vitest'
import {
  allSelected,
  defaultSelection,
  hasLossyTracks,
  isSelectable,
  parseTracks,
  readSelectionConfig,
  selectedNames,
  selectionStore,
  toggleAll,
  toggleTrack,
  type TrackInfo,
} from './selection'

const MIXED: TrackInfo[] = [
  { name: '01 A.flac', codec: 'flac', lossless: true },
  { name: '02 B.mp3', codec: 'mp3', lossless: false },
  { name: '03 C.flac', codec: 'flac', lossless: true },
]

const ALL_LOSSY: TrackInfo[] = [
  { name: '01 A.mp3', codec: 'mp3', lossless: false },
  { name: '02 B.mp3', codec: 'mp3', lossless: false },
]

describe('parseTracks', () => {
  it('parses the inline payload', () => {
    expect(parseTracks(JSON.stringify(MIXED))).toEqual(MIXED)
  })

  it('returns an empty list for missing, invalid or non-array content', () => {
    expect(parseTracks(null)).toEqual([])
    expect(parseTracks('')).toEqual([])
    expect(parseTracks('{oops')).toEqual([])
    expect(parseTracks('{"name":"x"}')).toEqual([])
  })

  it('drops entries without a name', () => {
    expect(parseTracks('[{"codec":"flac"},{"name":"ok.flac","codec":"flac","lossless":true}]')).toEqual([
      { name: 'ok.flac', codec: 'flac', lossless: true },
    ])
  })
})

describe('defaultSelection', () => {
  it('selects every lossless track and no lossy one', () => {
    expect(defaultSelection(MIXED)).toEqual({ '01 A.flac': true, '03 C.flac': true })
  })

  it('selects nothing in an all-lossy directory', () => {
    expect(defaultSelection(ALL_LOSSY)).toEqual({})
  })

  it('selects nothing in an empty directory', () => {
    expect(defaultSelection([])).toEqual({})
  })
})

describe('isSelectable', () => {
  it('accepts a lossless track', () => {
    expect(isSelectable(MIXED, '01 A.flac')).toBe(true)
  })

  it('rejects a lossy track and an unknown name', () => {
    expect(isSelectable(MIXED, '02 B.mp3')).toBe(false)
    expect(isSelectable(MIXED, 'nope.flac')).toBe(false)
  })
})

describe('toggleTrack', () => {
  it('deselects a selected lossless track', () => {
    const next = toggleTrack(MIXED, defaultSelection(MIXED), '01 A.flac')
    expect(next['01 A.flac']).toBe(false)
    expect(next['03 C.flac']).toBe(true)
  })

  it('reselects a deselected lossless track', () => {
    expect(toggleTrack(MIXED, {}, '03 C.flac')['03 C.flac']).toBe(true)
  })

  it('never selects a lossy track', () => {
    expect(toggleTrack(MIXED, {}, '02 B.mp3')).toEqual({})
  })

  it('ignores an unknown name', () => {
    expect(toggleTrack(MIXED, {}, 'nope.flac')).toEqual({})
  })

  it('does not mutate the input map', () => {
    const before = defaultSelection(MIXED)
    toggleTrack(MIXED, before, '01 A.flac')
    expect(before['01 A.flac']).toBe(true)
  })
})

describe('allSelected', () => {
  it('is true when every lossless track is selected, ignoring lossy ones', () => {
    expect(allSelected(MIXED, defaultSelection(MIXED))).toBe(true)
  })

  it('is false when one lossless track is deselected', () => {
    expect(allSelected(MIXED, toggleTrack(MIXED, defaultSelection(MIXED), '01 A.flac'))).toBe(false)
  })

  it('is false when there is nothing selectable', () => {
    expect(allSelected(ALL_LOSSY, {})).toBe(false)
    expect(allSelected([], {})).toBe(false)
  })
})

describe('toggleAll', () => {
  it('clears the selection when everything lossless is selected', () => {
    expect(toggleAll(MIXED, defaultSelection(MIXED))).toEqual({})
  })

  it('selects every lossless track from a partial selection', () => {
    expect(toggleAll(MIXED, { '01 A.flac': true })).toEqual({ '01 A.flac': true, '03 C.flac': true })
  })

  it('selects nothing in an all-lossy directory', () => {
    expect(toggleAll(ALL_LOSSY, {})).toEqual({})
  })
})

describe('selectedNames', () => {
  it('returns the selected lossless names in directory order', () => {
    expect(selectedNames(MIXED, defaultSelection(MIXED))).toEqual(['01 A.flac', '03 C.flac'])
  })

  it('omits a deselected track', () => {
    expect(selectedNames(MIXED, { '03 C.flac': true })).toEqual(['03 C.flac'])
  })

  it('never returns a lossy track, even if the map claims it is selected', () => {
    expect(selectedNames(MIXED, { '02 B.mp3': true })).toEqual([])
  })

  it('returns an empty list when nothing is selected', () => {
    expect(selectedNames(MIXED, {})).toEqual([])
  })
})

describe('readSelectionConfig', () => {
  it('reads the path from the dock and the tracks from the inline tag', () => {
    document.body.innerHTML = `
      <aside id="tc-dock" data-path="/music/Album"></aside>
      <script type="application/json" id="tc-tracks-data">${JSON.stringify(MIXED)}</script>`
    expect(readSelectionConfig(document)).toEqual({ path: '/music/Album', tracks: MIXED })
  })

  it('reports an empty directory when the tags are absent', () => {
    document.body.innerHTML = ''
    expect(readSelectionConfig(document)).toEqual({ path: '', tracks: [] })
  })
})

describe('hasLossyTracks', () => {
  it('is true only when some track is lossy', () => {
    expect(hasLossyTracks(MIXED)).toBe(true)
    expect(hasLossyTracks(ALL_LOSSY)).toBe(true)
    expect(hasLossyTracks([{ name: '01 A.flac', codec: 'flac', lossless: true }])).toBe(false)
    expect(hasLossyTracks([])).toBe(false)
  })
})

describe('selectionStore', () => {
  it('starts with every lossless track selected', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    expect(store.names).toEqual(['01 A.flac', '03 C.flac'])
    expect(store.losslessCount).toBe(2)
    expect(store.allSelected).toBe(true)
  })

  it('keeps the selection when re-initialised with the same path', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.toggle('01 A.flac')
    store.init('/music/Album', MIXED)
    expect(store.names).toEqual(['03 C.flac'])
  })

  it('resets the selection when the path changes', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.toggle('01 A.flac')
    store.init('/music/Other', MIXED)
    expect(store.names).toEqual(['01 A.flac', '03 C.flac'])
  })

  it('reports selectability and selected state per track', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    expect(store.isSelectable('01 A.flac')).toBe(true)
    expect(store.isSelectable('02 B.mp3')).toBe(false)
    expect(store.isSelected('02 B.mp3')).toBe(false)
  })

  it('never selects a lossy track through toggle', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.toggle('02 B.mp3')
    expect(store.isSelected('02 B.mp3')).toBe(false)
    expect(store.names).toEqual(['01 A.flac', '03 C.flac'])
  })

  it('toggles all off and back on', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.toggleAll()
    expect(store.names).toEqual([])
    expect(store.allSelected).toBe(false)
    store.toggleAll()
    expect(store.names).toEqual(['01 A.flac', '03 C.flac'])
  })

  it('defaults skip-lossy on for a mixed directory and off for an all-lossless one', () => {
    const mixed = selectionStore()
    mixed.init('/music/Album', MIXED)
    expect(mixed.hasLossy).toBe(true)
    expect(mixed.skipLossy).toBe(true)

    const lossless = selectionStore()
    lossless.init('/music/Lossless', [{ name: '01 A.flac', codec: 'flac', lossless: true }])
    expect(lossless.hasLossy).toBe(false)
    expect(lossless.skipLossy).toBe(false)
  })

  // skip_lossy and files are mutually exclusive server-side (400 invalid_request),
  // so an explicit per-row choice has to drop the toggle.
  it('clears skip-lossy when a row or the header checkbox is touched', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.toggle('01 A.flac')
    expect(store.skipLossy).toBe(false)

    store.setSkipLossy(true)
    store.toggleAll()
    expect(store.skipLossy).toBe(false)
  })

  it('restores the full lossless selection when skip-lossy is set back on', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.toggle('01 A.flac')
    expect(store.names).toEqual(['03 C.flac'])
    store.setSkipLossy(true)
    expect(store.names).toEqual(['01 A.flac', '03 C.flac'])
  })

  it('leaves the selection alone when skip-lossy is switched off', () => {
    const store = selectionStore()
    store.init('/music/Album', MIXED)
    store.setSkipLossy(false)
    expect(store.skipLossy).toBe(false)
    expect(store.names).toEqual(['01 A.flac', '03 C.flac'])
  })

  it('has nothing selectable in an all-lossy directory', () => {
    const store = selectionStore()
    store.init('/music/Lossy', ALL_LOSSY)
    expect(store.losslessCount).toBe(0)
    expect(store.allSelected).toBe(false)
    store.toggleAll()
    expect(store.names).toEqual([])
  })
})
