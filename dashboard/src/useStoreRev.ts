import { useSyncExternalStore } from 'react'
import type { Store } from './store'

/** Подписка на ревизию стора: перерисовка панели при любом событии ленты. */
export function useStoreRev(store: Store): number {
  return useSyncExternalStore(store.subscribe, store.getSnapshot)
}
