/**
 * Mobile/tablet responsive state, driven entirely by `<body>` classes.
 *
 * The `<body>` element survives htmx content swaps, so classes set on it persist
 * across navigation; `responsive.css` keys off them and this module toggles them
 * via document-level event delegation (robust against `#content-area` swaps):
 *
 * - `body.tree-open`  → the sidebar directory drawer is visible
 * - `body.dock-open`  → the transcode dock overlay is visible
 * - `body.has-sel`    → a directory with tracks is shown ⇒ the "Transcode" FAB appears
 * - `body.queue-open` → the queue is expanded (bridged from Alpine in base.html)
 */

/**
 * Pure helper: true when the transcode dock (`#tc-dock`) is present in the given
 * root (the content area, or the document). Used to decide `body.has-sel`.
 */
export function hasSelection(root: ParentNode | null | undefined): boolean {
  return !!root?.querySelector('#tc-dock')
}

/**
 * Recompute `body.has-sel` from the presence of `#tc-dock` in `root`, and always
 * clear `body.dock-open` — navigating to a new directory replaces the dock DOM,
 * so any previously-open overlay must not linger as an orphaned body class.
 */
export function syncSelection(root: ParentNode | null | undefined): void {
  document.body.classList.toggle('has-sel', hasSelection(root))
  document.body.classList.remove('dock-open')
}

/**
 * Wire the document-delegated listeners for the persistent mobile controls. The
 * trigger buttons (hamburger, FAB) live in `base.html` while their targets (tree
 * drawer, dock) may live in swapped-in content, so we delegate from `document`
 * and drive everything through `<body>` classes rather than direct element refs.
 */
let controlsWired = false

export function initMobileControls(): void {
  if (controlsWired) return
  controlsWired = true
  document.addEventListener('click', (event) => {
    const target = event.target as HTMLElement | null
    if (!target) return
    const body = document.body

    if (target.closest('.m-hamburger')) {
      // Opening the tree drawer closes any open dock overlay (one panel at a time).
      body.classList.remove('dock-open')
      body.classList.toggle('tree-open')
    } else if (target.closest('.m-transcode')) {
      body.classList.add('dock-open')
    } else if (target.closest('.scrim')) {
      body.classList.remove('tree-open', 'dock-open')
    } else if (target.closest('.tree-x')) {
      body.classList.remove('tree-open')
    } else if (target.closest('.dock-x')) {
      body.classList.remove('dock-open')
    }
  })
}
