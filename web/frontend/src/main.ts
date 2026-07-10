import Alpine from 'alpinejs'
import htmx from 'htmx.org'
import { reinitAlpineOnSwap } from './alpine-swap'
import { altoDock } from './dock'
import { altoLibMenu } from './libmenu'
import { altoQueue } from './queue'
import { altoTreeSearch } from './treesearch'
import { initSidebarResizer } from './ui/resizer'
import './styles/index.css'

declare global {
  interface Window {
    Alpine: typeof Alpine
    htmx: typeof htmx
  }
}

window.Alpine = Alpine
window.htmx = htmx

// Shared, reactive job state so the transcode dock (a separate Alpine island)
// can tell whether the album it's showing already has an active queue job and
// disable START. The global queue panel (queue.ts) keeps activeDirs current.
interface JobsStore {
  activeDirs: string[]
  isActive(dir: string): boolean
}
Alpine.store('jobs', {
  activeDirs: [] as string[],
  isActive(this: JobsStore, dir: string): boolean {
    return !!dir && this.activeDirs.includes(dir)
  },
})

Alpine.data('altoDock', altoDock)
Alpine.data('altoLibMenu', altoLibMenu)
Alpine.data('altoQueue', altoQueue)
Alpine.data('altoTreeSearch', altoTreeSearch)

// Tree album labels are real <a href> links so every browser link shortcut works
// (⌘/Ctrl/middle-click → new tab, Shift → new window, Alt → download). htmx
// preventDefaults every anchor click, so the link is bound to a custom `altonav`
// event instead of `click`; we fire that only for an unmodified left-click and
// drive the in-page swap, letting all modified/middle/aux clicks fall through to
// the browser untouched.
document.addEventListener('click', (event) => {
  const target = event.target as HTMLElement | null
  const link = target?.closest<HTMLAnchorElement>('a.tree-label-link')
  if (!link) return
  if (
    event.defaultPrevented ||
    event.button !== 0 ||
    event.metaKey ||
    event.ctrlKey ||
    event.shiftKey ||
    event.altKey
  ) {
    return // let the browser handle new-tab / new-window / download natively
  }
  event.preventDefault()
  document.querySelectorAll('.tree-node-row').forEach((el) => el.classList.remove('active'))
  link.closest('.tree-node-row')?.classList.add('active')
  htmx.trigger(link, 'altonav', {})
})

document.addEventListener('htmx:afterSwap', (event) => {
  reinitAlpineOnSwap(Alpine, (event as CustomEvent<{ target: EventTarget }>).detail.target)
})

// Restoring a page from htmx's history cache (back/forward) drops the swapped-in
// HTML straight into the DOM without firing htmx:afterSwap, leaving Alpine
// islands (e.g. the transcode dock) inert. Re-init the content area so its
// controls stay live after navigation.
document.addEventListener('htmx:historyRestore', () => {
  reinitAlpineOnSwap(Alpine, document.getElementById('content-area'))
})

initSidebarResizer()
Alpine.start()
