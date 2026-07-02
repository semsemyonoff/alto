import type Alpine from 'alpinejs'

/**
 * htmx swaps DOM subtrees outside Alpine's mutation-observer reach in some
 * cases; re-running Alpine.initTree on the swapped-in root keeps islands in
 * newly-loaded fragments (e.g. the directory page) interactive after
 * `htmx:afterSwap`.
 */
export function reinitAlpineOnSwap(alpine: Pick<typeof Alpine, 'initTree'>, target: EventTarget | null): void {
  if (target instanceof HTMLElement) {
    alpine.initTree(target)
  }
}
