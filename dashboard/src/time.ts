export function formatClock(ts: number): string {
  if (!ts) return '--:--:--'
  const d = new Date((ts + 3 * 3600) * 1000) // потоковое время — МСК
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`
}
