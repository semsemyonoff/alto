import { describe, expect, it, vi } from 'vitest'
import { clampWidth, initSidebarResizer, persistWidth, readStoredWidth } from './resizer'

describe('clampWidth', () => {
  it('passes widths inside the range through unchanged', () => {
    expect(clampWidth(300, 220, 560)).toBe(300)
  })

  it('clamps below the minimum', () => {
    expect(clampWidth(100, 220, 560)).toBe(220)
  })

  it('clamps above the maximum', () => {
    expect(clampWidth(900, 220, 560)).toBe(560)
  })

  it('rounds fractional widths', () => {
    expect(clampWidth(300.6, 220, 560)).toBe(301)
  })
})

describe('readStoredWidth', () => {
  it('parses a stored numeric width', () => {
    const storage = { getItem: vi.fn().mockReturnValue('340') }
    expect(readStoredWidth(storage)).toBe(340)
  })

  it('returns null when nothing is stored', () => {
    const storage = { getItem: vi.fn().mockReturnValue(null) }
    expect(readStoredWidth(storage)).toBeNull()
  })

  it('returns null for unparsable values', () => {
    const storage = { getItem: vi.fn().mockReturnValue('not-a-number') }
    expect(readStoredWidth(storage)).toBeNull()
  })

  it('returns null when storage access throws', () => {
    const storage = {
      getItem: vi.fn(() => {
        throw new Error('blocked')
      }),
    }
    expect(readStoredWidth(storage)).toBeNull()
  })
})

describe('persistWidth', () => {
  it('stores the rounded width under the sidebar width key', () => {
    const storage = { setItem: vi.fn() }
    persistWidth(storage, 312.4)
    expect(storage.setItem).toHaveBeenCalledWith('alto.sidebar.width', '312')
  })

  it('swallows storage errors', () => {
    const storage = {
      setItem: vi.fn(() => {
        throw new Error('blocked')
      }),
    }
    expect(() => persistWidth(storage, 300)).not.toThrow()
  })
})

describe('initSidebarResizer', () => {
  function buildDom(): Document {
    document.body.innerHTML = `
      <div class="shell">
        <div class="app-sidebar-resizer" tabindex="0"></div>
      </div>
    `
    // jsdom doesn't lay out elements, so stub realistic rects for the
    // computeMaxWidth() clamp math (shell width minus gutter minus content).
    const shell = document.querySelector<HTMLElement>('.shell')!
    const handle = document.querySelector<HTMLElement>('.app-sidebar-resizer')!
    shell.getBoundingClientRect = () => ({ width: 1200, left: 0 }) as unknown as DOMRect
    handle.getBoundingClientRect = () => ({ width: 10, left: 280 }) as unknown as DOMRect
    return document
  }

  it('applies the stored width on init', () => {
    const doc = buildDom()
    const shell = doc.querySelector<HTMLElement>('.shell')!
    const storage = { getItem: vi.fn().mockReturnValue('340'), setItem: vi.fn() }

    initSidebarResizer(doc, { storage })

    expect(shell.style.getPropertyValue('--sidebar-width')).toBe('340px')
  })

  it('falls back to the default width when nothing is stored', () => {
    const doc = buildDom()
    const shell = doc.querySelector<HTMLElement>('.shell')!
    const storage = { getItem: vi.fn().mockReturnValue(null), setItem: vi.fn() }

    initSidebarResizer(doc, { storage, defaultWidth: 280 })

    expect(shell.style.getPropertyValue('--sidebar-width')).toBe('280px')
  })

  it('resets to the default width on double-click and persists it', () => {
    const doc = buildDom()
    const shell = doc.querySelector<HTMLElement>('.shell')!
    const handle = doc.querySelector<HTMLElement>('.app-sidebar-resizer')!
    const storage = { getItem: vi.fn().mockReturnValue('500'), setItem: vi.fn() }

    initSidebarResizer(doc, { storage, defaultWidth: 280 })
    handle.dispatchEvent(new MouseEvent('dblclick', { bubbles: true }))

    expect(shell.style.getPropertyValue('--sidebar-width')).toBe('280px')
    expect(storage.setItem).toHaveBeenCalledWith('alto.sidebar.width', '280')
  })

  it('expands by the keyboard step on ArrowRight and persists it', () => {
    const doc = buildDom()
    const shell = doc.querySelector<HTMLElement>('.shell')!
    const handle = doc.querySelector<HTMLElement>('.app-sidebar-resizer')!
    const storage = { getItem: vi.fn().mockReturnValue('280'), setItem: vi.fn() }

    initSidebarResizer(doc, { storage, defaultWidth: 280, maxWidth: 560 })
    handle.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true, cancelable: true }))

    expect(shell.style.getPropertyValue('--sidebar-width')).toBe('296px')
    expect(storage.setItem).toHaveBeenCalledWith('alto.sidebar.width', '296')
  })

  it('does nothing when the shell or handle is missing from the document', () => {
    document.body.innerHTML = ''
    const storage = { getItem: vi.fn(), setItem: vi.fn() }

    expect(() => initSidebarResizer(document, { storage })).not.toThrow()
    expect(storage.getItem).not.toHaveBeenCalled()
  })
})
