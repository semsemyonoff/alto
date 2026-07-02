/**
 * The sidebar directory search box: an Alpine island that forwards its
 * (debounced) input value to the topbar library menu, which loads matching
 * directories from GET /api/tree/{id}/search into #tree-root via HTMX.
 */

export interface TreeSearchData {
  onInput(value: string): void
}

/** Alpine.data factory registered as `altoTreeSearch` and referenced via `x-data="altoTreeSearch()"`. */
export function altoTreeSearch(): TreeSearchData {
  return {
    onInput(value: string) {
      window.altoLibraryMenu?.search(value)
    },
  }
}
