/**
 * Per-track selection for the directory page.
 *
 * The track table and the transcode dock are two separate Alpine islands, so the
 * selection lives in an Alpine store (`$store.selection`) the way job state lives
 * in `$store.jobs`. Track metadata comes from the page's inline
 * `<script id="tc-tracks-data">` tag (rendered by buildDirTracksJSON), not from
 * scraping the table.
 *
 * Lossy tracks are never selectable: POST /api/transcode rejects them with
 * `lossy_source_selected`, so the UI must not be able to assemble that request.
 * All decision logic lives in the pure functions below, unit-tested without Alpine.
 */

export interface TrackInfo {
  name: string
  codec: string
  lossless: boolean
}

/** name → selected. Absent keys are unselected; lossy names never appear. */
export type SelectionMap = Record<string, boolean>

/** Parses the JSON text of the #tc-tracks-data script tag, tolerating missing/invalid content. */
export function parseTracks(json: string | null | undefined): TrackInfo[] {
  if (!json) return []
  try {
    const parsed: unknown = JSON.parse(json)
    if (!Array.isArray(parsed)) return []
    return parsed.filter((t): t is TrackInfo => !!t && typeof (t as TrackInfo).name === 'string')
  } catch {
    return []
  }
}

/** Reports whether the directory holds at least one lossy track, i.e. whether skip-lossy applies. */
export function hasLossyTracks(tracks: TrackInfo[]): boolean {
  return tracks.some((t) => !t.lossless)
}

/** The selection a freshly-loaded directory starts with: every lossless track, nothing else. */
export function defaultSelection(tracks: TrackInfo[]): SelectionMap {
  const selection: SelectionMap = {}
  for (const track of tracks) {
    if (track.lossless) selection[track.name] = true
  }
  return selection
}

/** Reports whether `name` is a lossless track of this directory, i.e. whether it can be selected at all. */
export function isSelectable(tracks: TrackInfo[], name: string): boolean {
  return tracks.some((t) => t.name === name && t.lossless)
}

/** Flips one track, returning a new map. Unknown and lossy names are a no-op. */
export function toggleTrack(tracks: TrackInfo[], selection: SelectionMap, name: string): SelectionMap {
  if (!isSelectable(tracks, name)) return { ...selection }
  return { ...selection, [name]: !selection[name] }
}

/** True when every lossless track is selected (and there is at least one). */
export function allSelected(tracks: TrackInfo[], selection: SelectionMap): boolean {
  const lossless = tracks.filter((t) => t.lossless)
  return lossless.length > 0 && lossless.every((t) => !!selection[t.name])
}

/** Select-all / deselect-all: selects every lossless track unless they already all are. */
export function toggleAll(tracks: TrackInfo[], selection: SelectionMap): SelectionMap {
  return allSelected(tracks, selection) ? {} : defaultSelection(tracks)
}

/** The selected filenames in directory order — the `files` list POST /api/transcode expects. */
export function selectedNames(tracks: TrackInfo[], selection: SelectionMap): string[] {
  return tracks.filter((t) => t.lossless && selection[t.name]).map((t) => t.name)
}

export interface SelectionConfig {
  path: string
  tracks: TrackInfo[]
}

/** Reads the directory's path and track metadata from the page's inline data tags. */
export function readSelectionConfig(doc: Document = document): SelectionConfig {
  const dock = doc.getElementById('tc-dock')
  const tracksEl = doc.getElementById('tc-tracks-data')
  return {
    path: dock?.dataset.path ?? '',
    tracks: parseTracks(tracksEl?.textContent),
  }
}

export interface SelectionStore {
  path: string
  tracks: TrackInfo[]
  selected: SelectionMap
  skipLossy: boolean
  init(path: string, tracks: TrackInfo[]): void
  isSelectable(name: string): boolean
  isSelected(name: string): boolean
  toggle(name: string): void
  toggleAll(): void
  setSkipLossy(on: boolean): void
  readonly losslessCount: number
  readonly hasLossy: boolean
  readonly allSelected: boolean
  readonly names: string[]
}

/** Builds the object registered as `Alpine.store('selection')`. */
export function selectionStore(): SelectionStore {
  return {
    path: '',
    tracks: [],
    selected: {},
    // The dock's "Skip lossy" toggle. It lives here rather than in the dock
    // because it and the row checkboxes are the two mutually exclusive selection
    // inputs (`skip_lossy` vs `files`), and they render in two separate islands.
    skipLossy: false,

    // Rebuilding only when the path differs makes this safe to call after every
    // htmx swap: navigating to another directory resets the selection, while a
    // swap that left the content area alone (e.g. a #tree-root refresh) keeps it.
    init(path: string, tracks: TrackInfo[]) {
      if (path === this.path) return
      this.path = path
      this.tracks = tracks
      this.selected = defaultSelection(tracks)
      this.skipLossy = hasLossyTracks(tracks)
    },

    isSelectable(name: string): boolean {
      return isSelectable(this.tracks, name)
    },
    isSelected(name: string): boolean {
      return !!this.selected[name]
    },
    // Touching a checkbox means an explicit `files` list, so it clears the
    // toggle — the server rejects a request carrying both.
    toggle(name: string) {
      this.selected = toggleTrack(this.tracks, this.selected, name)
      this.skipLossy = false
    },
    toggleAll() {
      this.selected = toggleAll(this.tracks, this.selected)
      this.skipLossy = false
    },
    // Turning skip-lossy back on restores the selection it describes, so the
    // checkbox column always shows what the next START will actually transcode.
    setSkipLossy(on: boolean) {
      this.skipLossy = on
      if (on) this.selected = defaultSelection(this.tracks)
    },

    get losslessCount(): number {
      return this.tracks.filter((t) => t.lossless).length
    },
    get hasLossy(): boolean {
      return hasLossyTracks(this.tracks)
    },
    get allSelected(): boolean {
      return allSelected(this.tracks, this.selected)
    },
    get names(): string[] {
      return selectedNames(this.tracks, this.selected)
    },
  }
}
