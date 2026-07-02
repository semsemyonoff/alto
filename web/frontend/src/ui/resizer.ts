const STORAGE_KEY = 'alto.sidebar.width'
const DEFAULT_WIDTH = 280
const MIN_WIDTH = 220
const MAX_WIDTH = 560
const KEYBOARD_STEP = 16

export interface ResizerOptions {
  storage?: Pick<Storage, 'getItem' | 'setItem'>
  defaultWidth?: number
  minWidth?: number
  maxWidth?: number
}

/** Clamps a candidate sidebar width to [minWidth, maxWidth], rounded to the nearest pixel. */
export function clampWidth(width: number, minWidth: number, maxWidth: number): number {
  return Math.max(minWidth, Math.min(maxWidth, Math.round(width)))
}

/** Reads the persisted sidebar width, returning null if absent or unparsable. */
export function readStoredWidth(storage: Pick<Storage, 'getItem'>): number | null {
  try {
    const raw = storage.getItem(STORAGE_KEY)
    if (!raw) return null
    const value = parseInt(raw, 10)
    return Number.isNaN(value) ? null : value
  } catch {
    return null
  }
}

/** Persists the sidebar width, swallowing storage errors (e.g. private browsing). */
export function persistWidth(storage: Pick<Storage, 'setItem'>, width: number): void {
  try {
    storage.setItem(STORAGE_KEY, String(Math.round(width)))
  } catch {
    // ignore
  }
}

/**
 * Wires pointer-drag, keyboard, and double-click resizing for the app sidebar,
 * persisting the chosen width in localStorage across reloads.
 */
export function initSidebarResizer(doc: Document = document, options: ResizerOptions = {}): void {
  const shell = doc.querySelector<HTMLElement>('.shell')
  const handle = doc.querySelector<HTMLElement>('.app-sidebar-resizer')
  if (!shell || !handle) return

  const storage = options.storage ?? window.localStorage
  const defaultWidth = options.defaultWidth ?? DEFAULT_WIDTH
  const minWidth = options.minWidth ?? MIN_WIDTH
  const maxWidth = options.maxWidth ?? MAX_WIDTH

  let currentWidth = defaultWidth
  let activePointerId: number | null = null

  function computeMaxWidth(): number {
    const shellWidth = Math.round(shell!.getBoundingClientRect().width)
    const gutterWidth = Math.round(handle!.getBoundingClientRect().width) || 10
    return Math.max(minWidth, Math.min(maxWidth, shellWidth - gutterWidth - 320))
  }

  function clamp(width: number): number {
    return clampWidth(width, minWidth, computeMaxWidth())
  }

  function syncAria(width: number): void {
    handle!.setAttribute('aria-valuemin', String(minWidth))
    handle!.setAttribute('aria-valuemax', String(computeMaxWidth()))
    handle!.setAttribute('aria-valuenow', String(width))
    handle!.setAttribute('aria-valuetext', `${width} pixels`)
  }

  function applyWidth(width: number, persist: boolean): void {
    currentWidth = clamp(width)
    shell!.style.setProperty('--sidebar-width', `${currentWidth}px`)
    syncAria(currentWidth)
    if (persist) persistWidth(storage, currentWidth)
  }

  function widthFromPointer(event: PointerEvent): number {
    return event.clientX - shell!.getBoundingClientRect().left
  }

  function cleanupDrag(): void {
    if (activePointerId === null) return
    if (handle!.hasPointerCapture?.(activePointerId)) {
      handle!.releasePointerCapture(activePointerId)
    }
    activePointerId = null
    handle!.classList.remove('dragging')
    doc.body.classList.remove('sidebar-resizing')
    applyWidth(currentWidth, true)
  }

  handle.addEventListener('pointerdown', (event) => {
    if (event.button !== undefined && event.button !== 0) return
    activePointerId = event.pointerId
    handle!.setPointerCapture?.(event.pointerId)
    handle!.classList.add('dragging')
    doc.body.classList.add('sidebar-resizing')
    handle!.focus()
    event.preventDefault()
  })

  handle.addEventListener('pointermove', (event) => {
    if (event.pointerId !== activePointerId) return
    applyWidth(widthFromPointer(event), false)
  })

  handle.addEventListener('pointerup', (event) => {
    if (event.pointerId !== activePointerId) return
    cleanupDrag()
  })

  handle.addEventListener('pointercancel', (event) => {
    if (event.pointerId !== activePointerId) return
    cleanupDrag()
  })

  handle.addEventListener('dblclick', () => {
    applyWidth(defaultWidth, true)
  })

  handle.addEventListener('keydown', (event) => {
    const step = event.shiftKey ? KEYBOARD_STEP * 2 : KEYBOARD_STEP
    switch (event.key) {
      case 'ArrowLeft':
        applyWidth(currentWidth - step, true)
        event.preventDefault()
        break
      case 'ArrowRight':
        applyWidth(currentWidth + step, true)
        event.preventDefault()
        break
      case 'Home':
        applyWidth(minWidth, true)
        event.preventDefault()
        break
      case 'End':
        applyWidth(computeMaxWidth(), true)
        event.preventDefault()
        break
    }
  })

  window.addEventListener('resize', () => applyWidth(currentWidth, false))
  window.addEventListener('blur', cleanupDrag)

  applyWidth(readStoredWidth(storage) ?? defaultWidth, false)
}
