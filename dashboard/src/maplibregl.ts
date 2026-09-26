import { setWorkerUrl } from 'maplibre-gl'
// maplibre ищет свой воркер как `new URL(`./${t}`, import.meta.url)` — с
// переменной частью, поэтому сборщик не может найти файл статически и в
// production-сборку он просто не попадает (в dev его терял dep-оптимизатор
// Vite, см. optimizeDeps.exclude в vite.config.ts). Импортируем воркер как
// ассет явно: и dev, и prod получают настоящий URL. Модуль существует ради
// этого побочного эффекта, поэтому импортировать его нужно до создания Map.
import workerUrl from 'maplibre-gl/dist/maplibre-gl-worker.mjs?worker&url'

setWorkerUrl(workerUrl)
