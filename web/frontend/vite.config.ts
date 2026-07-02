import { defineConfig } from 'vite'
import { resolve } from 'node:path'

export default defineConfig({
  root: __dirname,
  base: '/static/dist/',
  build: {
    manifest: true,
    outDir: resolve(__dirname, '../static/dist'),
    emptyOutDir: true,
    rollupOptions: {
      input: resolve(__dirname, 'src/main.ts'),
    },
  },
  test: {
    environment: 'jsdom',
  },
})
