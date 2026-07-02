import Alpine from 'alpinejs'
import htmx from 'htmx.org'
import { reinitAlpineOnSwap } from './alpine-swap'
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

document.addEventListener('htmx:afterSwap', (event) => {
  reinitAlpineOnSwap(Alpine, (event as CustomEvent<{ target: EventTarget }>).detail.target)
})

initSidebarResizer()
Alpine.start()
