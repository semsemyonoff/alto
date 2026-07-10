import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { hasSelection, initMobileControls, syncSelection } from './mobile'

function makeRoot(withDock: boolean): HTMLElement {
  const root = document.createElement('main')
  if (withDock) {
    const dock = document.createElement('aside')
    dock.id = 'tc-dock'
    root.appendChild(dock)
  }
  return root
}

afterEach(() => {
  document.body.className = ''
  document.body.innerHTML = ''
})

describe('hasSelection', () => {
  it('is true when #tc-dock is present in the root', () => {
    expect(hasSelection(makeRoot(true))).toBe(true)
  })

  it('is false when #tc-dock is absent', () => {
    expect(hasSelection(makeRoot(false))).toBe(false)
  })

  it('is false for a null/undefined root', () => {
    expect(hasSelection(null)).toBe(false)
    expect(hasSelection(undefined)).toBe(false)
  })
})

describe('syncSelection', () => {
  it('sets body.has-sel when the dock is present', () => {
    syncSelection(makeRoot(true))
    expect(document.body.classList.contains('has-sel')).toBe(true)
  })

  it('clears body.has-sel when the dock is absent', () => {
    document.body.classList.add('has-sel')
    syncSelection(makeRoot(false))
    expect(document.body.classList.contains('has-sel')).toBe(false)
  })

  it('always clears a lingering dock-open on selection change', () => {
    document.body.classList.add('dock-open')
    syncSelection(makeRoot(true))
    expect(document.body.classList.contains('dock-open')).toBe(false)
  })
})

describe('initMobileControls', () => {
  beforeEach(() => {
    initMobileControls()
  })

  function click(html: string): void {
    document.body.innerHTML = html
    ;(document.body.firstElementChild as HTMLElement).click()
  }

  it('hamburger toggles tree-open and clears dock-open', () => {
    document.body.classList.add('dock-open')
    click('<button class="m-hamburger"></button>')
    expect(document.body.classList.contains('tree-open')).toBe(true)
    expect(document.body.classList.contains('dock-open')).toBe(false)

    document.body.innerHTML = '<button class="m-hamburger"></button>'
    ;(document.body.firstElementChild as HTMLElement).click()
    expect(document.body.classList.contains('tree-open')).toBe(false)
  })

  it('FAB opens the dock', () => {
    click('<button class="m-transcode"></button>')
    expect(document.body.classList.contains('dock-open')).toBe(true)
  })

  it('scrim closes both tree and dock', () => {
    document.body.classList.add('tree-open', 'dock-open')
    click('<div class="scrim"></div>')
    expect(document.body.classList.contains('tree-open')).toBe(false)
    expect(document.body.classList.contains('dock-open')).toBe(false)
  })

  it('tree-x closes the tree only', () => {
    document.body.classList.add('tree-open', 'dock-open')
    click('<button class="tree-x"></button>')
    expect(document.body.classList.contains('tree-open')).toBe(false)
    expect(document.body.classList.contains('dock-open')).toBe(true)
  })

  it('dock-x closes the dock only', () => {
    document.body.classList.add('tree-open', 'dock-open')
    click('<button class="dock-x"></button>')
    expect(document.body.classList.contains('dock-open')).toBe(false)
    expect(document.body.classList.contains('tree-open')).toBe(true)
  })

  it('fires when the click lands on a child of the control', () => {
    click('<button class="m-hamburger"><span>bar</span></button>')
    expect(document.body.classList.contains('tree-open')).toBe(true)
  })
})
