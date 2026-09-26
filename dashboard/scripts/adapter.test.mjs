// Unit-тесты live-адаптера wire → контракт (GitLab #36, этап 5).
// Раннер — node:test из stdlib (в проекте браузерного тест-раннера нет,
// заводить новый ради одной проверки не стали). Тестируется ТОТ ЖЕ код,
// что исполняется в браузере: бандл scripts/adapter.bundle.mjs собран
// esbuild из src/wire.ts, src/store.ts, src/risk.ts.
//
//   node scripts/build-adapter.mjs && node --test scripts/adapter.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { WireAdapter, Store, vehicleRisk } from './adapter.bundle.mjs'

const AT = '2026-09-26T04:00:00+03:00'
const AT_SAW = '2026-09-26T04:00:05.123+03:00'
const epoch = s => Date.parse(s) / 1000

function card(over = {}) {
  return {
    unit_id: 1166336, tr_id: 122658, has_tr_id: true,
    last_seen: AT_SAW, staleness_s: 0.2, stale: false,
    speed_kmh: 34, lat: 55.7336, lon: 37.5336, location_valid: true,
    course_deg: 90, satellites: 9, points_in_window: 12,
    prediction: {
      sample_id: '122658_2026-09-26T01:00:00Z', target_stop_id: 536,
      horizon_s: 720, delta_s: 12, predicted_dev_s: 132, p_late: 0.66,
      source: 'ml', stale: false, model_version: 'v1v3b',
      risk: 'red', as_of: AT,
    },
    risk: 'red', open_incident_id: 'inc-1',
    ...over,
  }
}

function wire(type, data, at = AT) {
  return JSON.stringify({ type, at, data })
}

function inc(over = {}) {
  return {
    id: 'inc-1', unit_id: 1166336, tr_id: 122658, target_stop_id: 536,
    status: 'open', risk: 'red', opened_at: AT, updated_at: AT_SAW,
    acked_at: null, predicted_dev_s: 132, p_late: 0.66, stale: false,
    prev_stop_id: 535,
    source: 'ml', ...over,
  }
}

test('vehicle_update: карточка с прогнозом даёт vehicle+frame+model с правильной осью времени', () => {
  const a = new WireAdapter()
  const { events, acked, resolved } = a.decodeMessage(wire('vehicle_update', card()))
  assert.deepEqual(acked, [])
  assert.deepEqual(resolved, [])
  const types = events.map(e => e.type)
  assert.deepEqual(types, ['vehicle', 'frame', 'model'])
  const [v, f, m] = events
  // точка телеметрии — по времени приёма (last_seen), кадр — по as_of прогноза;
  // обе метки — истинный unix epoch, зона съедена Date.parse без ручных сдвигов
  assert.equal(v.ts, epoch(AT_SAW))
  assert.equal(f.ts, epoch(AT))
  assert.equal(m.ts, epoch(AT_SAW)) // модель якорится к машине
  assert.equal(v.tr_id, 122658)
  assert.equal(v.lon, 37.5336)
  assert.equal(v.lat, 55.7336)
  assert.equal(v.speed, 34)
  assert.equal(v.heading, 90)
  assert.equal(f.sample_id, '122658_2026-09-26T01:00:00Z')
  assert.equal(f.target_stop_id, 536)
  assert.equal(f.horizon_s, 720)
  assert.equal(f.cur_dev_s, null) // подсказки организаторов в онлайне нет
  assert.equal(f.official, false)
  assert.equal(f.risk, 'red')
  assert.equal(f.p_late, 0.66)
  assert.equal(f.values.predicted_dev_s, 132)
  assert.equal(f.values.speed_current, 34)
  assert.equal(m.model_version, 'v1v3b')
  assert.match(m.version, /v1v3b/)
})

test('vehicle_update: «Z»-строка Go (UTC-зона процесса) парится в ту же ось epoch', () => {
  const a = new WireAdapter()
  const { events } = a.decodeMessage(wire('vehicle_update',
    { ...card(), last_seen: '2026-09-25T21:00:00Z' }))
  assert.equal(events[0].ts, Date.parse('2026-09-25T21:00:00Z') / 1000)
})

test('снимок {vehicles:[...]} разворачивается в карточки; model — одно событие на версию', () => {
  const a = new WireAdapter()
  const snap = wire('vehicle_update', { vehicles: [card(), card({ tr_id: 7, unit_id: 7 })], count: 2 })
  const r1 = a.decodeMessage(snap)
  assert.equal(r1.events.filter(e => e.type === 'vehicle').length, 2)
  assert.equal(r1.events.filter(e => e.type === 'model').length, 1) // не спам
  const r2 = a.decodeMessage(snap)
  assert.equal(r2.events.filter(e => e.type === 'model').length, 0) // версия не менялась
})

test('карточки без привязки tr_id и с недостоверной позицией не попадают в поток', () => {
  const a = new WireAdapter()
  assert.deepEqual(a.decodeMessage(wire('vehicle_update', card({ has_tr_id: false, tr_id: 0 }))).events, [])
  assert.deepEqual(a.decodeMessage(wire('vehicle_update', card({ location_valid: false }))).events, [])
  // карточка без прогноза — только телеметрия, риска нет
  const r = a.decodeMessage(wire('vehicle_update', card({ prediction: null })))
  assert.deepEqual(r.events.map(e => e.type), ['vehicle'])
})

test('incident: карточка инцидента → контрактное событие, горизонт из последнего прогноза машины', () => {
  const a = new WireAdapter()
  a.decodeMessage(wire('vehicle_update', card())) // сначала машина: Observe шлёт её раньше
  const { events, acked, resolved } = a.decodeMessage(
    wire('incident', { incident: inc(), stats: { total: 1, open: 1, acked: 0, resolved: 0 } }))
  assert.deepEqual(acked, [])
  assert.deepEqual(resolved, [])
  assert.equal(events.length, 1)
  const i = events[0]
  assert.equal(i.type, 'incident')
  assert.equal(i.id, 'inc-1')
  assert.equal(i.ts, epoch(AT_SAW)) // updated_at
  assert.equal(i.tr_id, 122658)
  assert.equal(i.target_stop_id, 536)
  assert.equal(i.horizon_s, 720)
  assert.equal(i.predicted_delay_s, 132)
  assert.equal(i.source, 'gateway:ml')
  assert.equal(i.prev_stop_id, 535) // участок «откуда опаздывают» для карточки
  assert.match(i.reason, /p_late 0\.66/)
})

test('incident: причина с gateway-провода важнее технической строки, p_late — полем', () => {
  const a = new WireAdapter()
  const { events } = a.decodeMessage(wire('incident', {
    incident: inc({ reason: 'длительный простой на остановке' }), stats: {},
  }))
  const i = events[0]
  assert.equal(i.reason, 'длительный простой на остановке')
  assert.equal(i.p_late, 0.66)
  // null от fallback-прогноза — возвращаемся к технической строке
  const { events: e2 } = a.decodeMessage(wire('incident', {
    incident: inc({ reason: null }), stats: {},
  }))
  assert.match(e2[0].reason, /p_late 0\.66/)
})

test('incident: acked помечается на подтверждение, resolved уходит из выдачи без события', () => {
  const a = new WireAdapter()
  const r1 = a.decodeMessage(wire('incident', { incident: inc({ status: 'acked', acked_at: AT }) }))
  assert.deepEqual(r1.acked, ['inc-1'])
  assert.equal(r1.events[0].id, 'inc-1')
  const r2 = a.decodeMessage(wire('incident', { incident: inc({ status: 'resolved' }) }))
  assert.deepEqual(r2.events, [])
  assert.deepEqual(r2.resolved, ['inc-1'])
})

test('metrics: срез снимка (массив конвертов) рекурсивно разворачивается в события', () => {
  const a = new WireAdapter()
  const inner = [
    { type: 'incident', at: AT, data: { incidents: [inc()], stats: {} } },
    { type: 'vehicle_update', at: AT, data: { vehicles: [card()], count: 1 } },
  ]
  const { events } = a.decodeMessage(wire('metrics', inner))
  assert.deepEqual(events.map(e => e.type), ['incident', 'vehicle', 'frame', 'model'])
  assert.equal(events[1].tr_id, 122658)
})

test('мусор и неизвестные типы игнорируются, поток жив', () => {
  const a = new WireAdapter()
  assert.deepEqual(a.decodeMessage('не json').events, [])
  assert.deepEqual(a.decodeMessage(wire('weather', { rain: true })).events, [])
  assert.deepEqual(a.decodeMessage(wire('metrics', null)).events, [])
  assert.deepEqual(a.decodeMessage('').events, [])
})

test('store: повторный инцидент того же id не сбрасывает ack (at-least-once ленты)', () => {
  const s = new Store()
  const a = new WireAdapter()
  for (const ev of a.decodeMessage(wire('incident', { incident: inc() })).events) s.apply(ev)
  s.ack('inc-1')
  for (const ev of a.decodeMessage(wire('incident', { incident: inc() })).events) s.apply(ev)
  assert.equal(s.incidents.get('inc-1').acked, true)
  assert.equal(s.openIncidents(), 0)
})

test('store: горизонт дописывается из последующего прогноза (снимок раньше карточек)', () => {
  const s = new Store()
  const a = new WireAdapter()
  for (const ev of a.decodeMessage(wire('incident', { incident: inc() })).events) s.apply(ev)
  assert.equal(s.incidents.get('inc-1').horizon_s, 0) // снимок: прогноз по машине ещё не приходил
  for (const ev of a.decodeMessage(wire('vehicle_update', card())).events) s.apply(ev)
  for (const ev of a.decodeMessage(wire('incident', { incident: inc() })).events) s.apply(ev)
  assert.equal(s.incidents.get('inc-1').horizon_s, 720)
  s.ack('inc-1')
  for (const ev of a.decodeMessage(wire('incident', { incident: inc() })).events) s.apply(ev)
  assert.equal(s.incidents.get('inc-1').acked, true) // merge горизонта ack не трогает
})

test('risk.ts: live-классификация gateway приоритетна, replay живёт на правиле-фолбэке', () => {
  const s = new Store()
  const a = new WireAdapter()
  for (const ev of a.decodeMessage(wire('vehicle_update', card())).events) s.apply(ev)
  const v = s.vehicles.get(122658)
  // predicted_dev=132 и risk=red → красный, clock в тот же момент — не stale
  assert.equal(vehicleRisk(v, s.clock), 'red')
  // жёлтый по классификации gateway при cur_dev_s=null (правило бы сказало «unknown»)
  for (const ev of a.decodeMessage(wire('vehicle_update',
    card({ risk: 'yellow',
      prediction: { ...card().prediction, risk: 'yellow', predicted_dev_s: 70 } }))).events) s.apply(ev)
  assert.equal(vehicleRisk(s.vehicles.get(122658), s.clock), 'yellow')
  // stale по-прежнему по часам потока
  assert.equal(vehicleRisk(s.vehicles.get(122658), s.clock + 300), 'stale')
  // replay-кадр без risk — старое правило cur_dev_s
  const s2 = new Store()
  s2.apply({ type: 'vehicle', ts: 1000, tr_id: 5, lon: 0, lat: 0, speed: 0, heading: 0 })
  s2.apply({ type: 'frame', ts: 1000, sample_id: 'x', tr_id: 5, target_stop_id: 1,
    horizon_s: 600, ambiguous: false, cur_dev_s: 90, official: true, values: {} })
  assert.equal(vehicleRisk(s2.vehicles.get(5), 1000), 'yellow')
  s2.apply({ type: 'frame', ts: 1001, sample_id: 'y', tr_id: 5, target_stop_id: 1,
    horizon_s: 600, ambiguous: false, cur_dev_s: null, official: false, values: {} })
  assert.equal(vehicleRisk(s2.vehicles.get(5), 1001), 'unknown')
})

test('edge: at без зоны («Go наивный MSK+Z»-ловушка) — сравнение с python-эталонной осью', () => {
  // python (make_dashboard_demo): naive стенные МСК → tzinfo=MSK → .timestamp()
  // = истинный epoch того же момента. Go time.Time в +03:00 даёт ровно этот же
  // epoch через Date.parse; «Z»-строка из наивного МСК (только в поле t кадров
  // features, в ленте такого нет) уехала бы на 3 часа — проверяем, что лента
  // этого не делает.
  const a = new WireAdapter()
  const { events } = a.decodeMessage(wire('vehicle_update',
    { ...card(), last_seen: '2026-01-06T08:00:00+03:00' }))
  assert.equal(events[0].ts, Date.UTC(2026, 0, 6, 5, 0, 0) / 1000)
})
