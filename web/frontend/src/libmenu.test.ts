import { beforeEach, describe, expect, it } from 'vitest'
import { deriveSelectedName, libraryStatusText, readLibMenuConfig, treeURL, type LibraryOption } from './libmenu'

const LIBS: LibraryOption[] = [
  { id: 1, name: 'Downloads', path: '/music/downloads', indexed: true, track_count: 4812 },
  { id: 2, name: 'Lossless', path: '/music/lossless', indexed: true, track_count: 1290 },
  { id: 3, name: 'New Drive', path: '/music/new', indexed: false, track_count: 0 },
]

describe('deriveSelectedName', () => {
  it('returns the name of the library matching selectedId', () => {
    expect(deriveSelectedName(LIBS, 2, 'fallback')).toBe('Lossless')
  })

  it('falls back when no library matches selectedId', () => {
    expect(deriveSelectedName(LIBS, 99, 'fallback')).toBe('fallback')
  })
})

describe('libraryStatusText', () => {
  it('formats an indexed library with a thousands-separated track count', () => {
    expect(libraryStatusText(LIBS[0])).toBe('indexed · 4,812 tracks')
  })

  it('reports "not indexed" for an unindexed library', () => {
    expect(libraryStatusText(LIBS[2])).toBe('not indexed')
  })

  it('reports "not indexed" when no library is given', () => {
    expect(libraryStatusText(undefined)).toBe('not indexed')
  })
})

describe('treeURL', () => {
  it('builds the children endpoint for a library id', () => {
    expect(treeURL(2)).toBe('/api/tree/2/children')
  })
})

describe('readLibMenuConfig', () => {
  beforeEach(() => {
    document.body.innerHTML = ''
  })

  it('reads the selected id/name from data attributes', () => {
    document.body.innerHTML = `<div id="libwrap" data-selected-id="2" data-selected-name="Lossless"></div>`
    const el = document.getElementById('libwrap') as HTMLElement
    expect(readLibMenuConfig(el)).toEqual({ selectedId: 2, selectedName: 'Lossless' })
  })

  it('defaults to id 0 and an empty name when attributes are missing', () => {
    document.body.innerHTML = `<div id="libwrap"></div>`
    const el = document.getElementById('libwrap') as HTMLElement
    expect(readLibMenuConfig(el)).toEqual({ selectedId: 0, selectedName: '' })
  })
})
