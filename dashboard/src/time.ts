// Ось времени потока — настоящий unix epoch: wall-clock МСК − 3 ч
// (см. parse_time в scripts/make_dashboard_demo.py). Смещение +3:00 ниже
// захардкожено осознанно: датасет = Москва, DST в РФ нет.
export function formatClock(ts: number): string {
  if (!ts) return '--:--:--'
  const d = new Date((ts + 3 * 3600) * 1000) // показываем стенное время МСК
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`
}
