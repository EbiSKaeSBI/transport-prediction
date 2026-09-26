import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  optimizeDeps: {
    // maplibre-gl нельзя пропускать через dep-оптимизатор: он склеивает
    // библиотеку в .vite/deps, но воркер maplibre-gl-worker.mjs туда не
    // попадает, и карта падает на new Worker() — «The file does not exist at
    // .../.vite/deps/maplibre-gl-worker.mjs». Воркер нужен всегда (глифы и
    // тайлы), даже при пустом офлайн-стиле, поэтому исключение обязательно:
    // без него падает весь дашборд, а не только карта.
    exclude: ['maplibre-gl'],
  },
  server: {
    proxy: {
      // живой контур (этап 4): gateway на :8080 — REST и WebSocket
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
      '/ws': { target: 'ws://localhost:8080', ws: true },
    },
  },
})
