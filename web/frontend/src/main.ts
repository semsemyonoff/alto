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

Alpine.data('altoDock', altoDock)
Alpine.data('altoLibMenu', altoLibMenu)
Alpine.data('altoQueue', altoQueue)
Alpine.data('altoTreeSearch', altoTreeSearch)

document.addEventListener('htmx:afterSwap', (event) => {
  reinitAlpineOnSwap(Alpine, (event as CustomEvent<{ target: EventTarget }>).detail.target)
})

initSidebarResizer()
Alpine.start()
