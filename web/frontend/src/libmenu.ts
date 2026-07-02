/**
 * The topbar library selector: an Alpine island that lists libraries with
 * their indexed/track-count status (from GET /api/libraries), switches the
 * selected library by loading its root tree via HTMX, and keeps the shared
 * #statusdot/#scan-text elements in the topbar in sync with the selection.
 * base.html's scan-status script bridges through `window.altoLibraryMenu`
 * to restore this status after a transient scan message hides.
 */

export interface LibraryOption {
  id: number
  name: string
  path: string
  indexed: boolean
  track_count: number
}

/** Finds the display name for the selected library, falling back to the given default. */
export function deriveSelectedName(libraries: LibraryOption[], selectedId: number, fallback: string): string {
  return libraries.find((l) => l.id === selectedId)?.name ?? fallback
}

/** The topbar status text for a library: "indexed · N tracks" or "not indexed". */
export function libraryStatusText(lib: LibraryOption | undefined): string {
  if (!lib || !lib.indexed) return 'not indexed'
  return `indexed · ${lib.track_count.toLocaleString('en-US')} tracks`
}

export function treeURL(libraryId: number): string {
  return `/api/tree/${libraryId}/children`
}

export function searchURL(libraryId: number, q: string): string {
  return `/api/tree/${libraryId}/search?q=${encodeURIComponent(q)}`
}

/** Loads a library's root tree fragment into #tree-root via the bundled htmx instance. */
function loadTree(libraryId: number): void {
  window.htmx?.ajax('GET', treeURL(libraryId), { target: '#tree-root', swap: 'innerHTML' })
}

/** Loads search results for q into #tree-root; an empty q restores the root tree. */
function loadSearch(libraryId: number, q: string): void {
  const trimmed = q.trim()
  if (!trimmed) {
    loadTree(libraryId)
    return
  }
  window.htmx?.ajax('GET', searchURL(libraryId, trimmed), { target: '#tree-root', swap: 'innerHTML' })
}

/** Reads the library menu's initial selection from the wrapping element's data attributes. */
export function readLibMenuConfig(el: HTMLElement): { selectedId: number; selectedName: string } {
  return {
    selectedId: Number(el.dataset.selectedId ?? '0'),
    selectedName: el.dataset.selectedName ?? '',
  }
}

interface LibMenuData {
  libraries: LibraryOption[]
  selectedId: number
  selectedName: string
  open: boolean
  init(): void
  load(): Promise<void>
  select(lib: LibraryOption): void
}

/** Applies a library's indexed status to the shared #statusdot/#scan-text elements. */
function applyStatus(lib: LibraryOption | undefined, doc: Document = document): void {
  const dot = doc.getElementById('statusdot')
  const text = doc.getElementById('scan-text')
  if (dot) dot.classList.toggle('off', !(lib && lib.indexed))
  if (text) text.textContent = libraryStatusText(lib)
}

/** Alpine.data factory registered as `altoLibMenu` and referenced via `x-data="altoLibMenu()"`. */
export function altoLibMenu(): LibMenuData {
  return {
    libraries: [],
    selectedId: 0,
    selectedName: '',
    open: false,

    init() {
      const el = (this as unknown as { $el: HTMLElement }).$el
      const config = readLibMenuConfig(el)
      this.selectedId = config.selectedId
      this.selectedName = config.selectedName

      window.altoLibraryMenu = {
        refreshStatus: () => void this.load(),
        reloadTree: () => loadTree(this.selectedId),
        search: (q) => loadSearch(this.selectedId, q),
      }

      void this.load()
    },

    async load() {
      try {
        const res = await fetch('/api/libraries')
        const data: { libraries?: LibraryOption[] } = await res.json().catch(() => ({}))
        this.libraries = data.libraries ?? []
        this.selectedName = deriveSelectedName(this.libraries, this.selectedId, this.selectedName)
      } catch {
        this.libraries = []
      }
      applyStatus(this.libraries.find((l) => l.id === this.selectedId))
    },

    select(lib: LibraryOption) {
      this.selectedId = lib.id
      this.selectedName = lib.name
      this.open = false
      applyStatus(lib)
      loadTree(lib.id)
    },
  }
}

declare global {
  interface Window {
    altoLibraryMenu?: {
      refreshStatus(): void
      reloadTree(): void
      search(q: string): void
    }
  }
}
