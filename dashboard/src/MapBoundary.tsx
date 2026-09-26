import { Component } from 'react'
import type { ErrorInfo, ReactNode } from 'react'

interface Props {
  children: ReactNode
  /** Сообщить родителю, что MapLibre не поднялся: переключаемся на canvas. */
  onFailure: (err: unknown) => void
}

interface State {
  failed: boolean
}

/**
 * Граница ошибок вокруг MapLibre.
 *
 * hasWebGL() проверяет только факт WebGL-контекста, но карта может упасть
 * уже после этого: воркер maplibre-gl, стиль, отсутствующий GL-функционал.
 * Без границы такое исключение из useEffect размонтирует всё дерево React —
 * вместе с карточкой прогноза и рельсом инцидентов, то есть отказывает не
 * карта, а весь дашборд. Здесь падение карты стоит ей карты: родитель
 * переключает рендерер на CanvasMap, и панели продолжают жить.
 */
export default class MapBoundary extends Component<Props, State> {
  state: State = { failed: false }

  static getDerivedStateFromError(): State {
    return { failed: true }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // причина и стек — в консоль, чтобы падение рендерера не было немым
    console.error('MapLibre не инициализировался, включён canvas-фолбэк', error, info.componentStack)
    this.props.onFailure(error)
  }

  render() {
    return this.state.failed ? null : this.props.children
  }
}
