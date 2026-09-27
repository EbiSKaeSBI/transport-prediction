// Яндекс JS API 3.0. Библиотека отдаётся только ссылкой с ключом и не
// хостится у нас (см. README пакета @yandex/ymaps3-types):
//   <script src="https://api-maps.yandex.ru/v3/?apikey=…&lang=ru_RU">
// Компоненты появляются в глобальном `ymaps3` после резолва `ymaps3.ready`.
//
// Типы ниже — узкое подмножество нужного дашборду; полная контрактная
// картина в node_modules/@yandex/ymaps3-types (dev-зависимость).

export type EntityProps = Record<string, unknown>

export interface YEntity {
  update(props: EntityProps): unknown
  addChild(child: YEntity): unknown
  removeChild(child: YEntity): unknown
  destroy(): void
}

export interface YMaps3 {
  ready: Promise<void>
  YMap: new (container: HTMLElement, props: EntityProps) => YEntity
  YMapDefaultSchemeLayer: new (props?: EntityProps) => YEntity
  YMapDefaultFeaturesLayer: new (props?: EntityProps) => YEntity
  YMapFeature: new (props: EntityProps) => YEntity
  YMapMarker: new (props: EntityProps) => YEntity
}

const LOADER_URL = 'https://api-maps.yandex.ru/v3/'

/** Ключ из окружения (dashboard/.env, gitignore). Пусто — Яндекс не включаем. */
export function ymapsApiKey(): string {
  const raw = import.meta.env.VITE_YANDEX_MAPS_API_KEY
  return typeof raw === 'string' ? raw.trim() : ''
}

let loading: Promise<YMaps3> | null = null

/**
 * Грузит скрипт API ровно один раз на страницу (промис кэшируется).
 * Битый/неактивированный ключ или отсутствие сети — reject, и MapView
 * тихо откатывается на привычный офлайн-движок MapLibre.
 */
export function loadYmaps(lang = 'ru_RU'): Promise<YMaps3> {
  if (loading) return loading
  const key = ymapsApiKey()
  if (!key) return Promise.reject(new Error('VITE_YANDEX_MAPS_API_KEY пуст'))
  loading = new Promise<YMaps3>((resolve, reject) => {
    const fail = (why: string) => {
      loading = null
      script.remove()
      reject(new Error(`Яндекс JS API: ${why}`))
    }
    const script = document.createElement('script')
    script.src = `${LOADER_URL}?apikey=${encodeURIComponent(key)}&lang=${encodeURIComponent(lang)}`
    script.async = true
    script.onerror = () => fail('скрипт не загрузился (ключ отклонён или нет сети)')
    script.onload = () => {
      // 403 на битый ключ приходит как JSON: скрипт «загрузился», но
      // глобалы нет — проверяем явно, чтобы не зависеть на ready.
      const api = (window as unknown as { ymaps3?: YMaps3 }).ymaps3
      if (!api) return fail('глобал ymaps3 не появился (ключ отклонён?)')
      api.ready.then(() => resolve(api), () => fail('модули API не поднялись'))
    }
    document.head.appendChild(script)
  })
  return loading
}
