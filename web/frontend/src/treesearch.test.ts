import { afterEach, describe, expect, it, vi } from 'vitest'
import { altoTreeSearch } from './treesearch'

describe('altoTreeSearch', () => {
  afterEach(() => {
    delete window.altoLibraryMenu
  })

  it('forwards the input value to window.altoLibraryMenu.search', () => {
    const search = vi.fn()
    window.altoLibraryMenu = { refreshStatus: vi.fn(), reloadTree: vi.fn(), search }

    altoTreeSearch().onInput('miles')

    expect(search).toHaveBeenCalledWith('miles')
  })

  it('does nothing when the library menu has not initialized yet', () => {
    expect(() => altoTreeSearch().onInput('miles')).not.toThrow()
  })
})
