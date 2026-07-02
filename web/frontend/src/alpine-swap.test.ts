import { describe, expect, it, vi } from 'vitest'
import { reinitAlpineOnSwap } from './alpine-swap'

describe('reinitAlpineOnSwap', () => {
  it('re-initializes Alpine on the swapped-in element', () => {
    const alpine = { initTree: vi.fn() }
    const target = document.createElement('div')

    reinitAlpineOnSwap(alpine, target)

    expect(alpine.initTree).toHaveBeenCalledTimes(1)
    expect(alpine.initTree).toHaveBeenCalledWith(target)
  })

  it('does nothing when the event target is not an element', () => {
    const alpine = { initTree: vi.fn() }

    reinitAlpineOnSwap(alpine, null)

    expect(alpine.initTree).not.toHaveBeenCalled()
  })
})
