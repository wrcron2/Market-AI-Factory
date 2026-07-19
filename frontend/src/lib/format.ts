// ─── Number / time formatting helpers ────────────────────────────────────────

export const fmtUSD = (n: number, dp = 2): string =>
  n.toLocaleString('en-US', {
    style: 'currency',
    currency: 'USD',
    minimumFractionDigits: dp,
    maximumFractionDigits: dp,
  })

export const fmtNum = (n: number, dp = 2): string =>
  n.toLocaleString('en-US', { minimumFractionDigits: dp, maximumFractionDigits: dp })

export const fmtSignedUSD = (n: number, dp = 2): string =>
  `${n >= 0 ? '+' : '-'}${fmtUSD(Math.abs(n), dp)}`

export const fmtPct = (n: number, dp = 1): string => `${n >= 0 ? '+' : ''}${n.toFixed(dp)}%`

/** Confidence value is 0..1 (matches StagedOrder.confidence). */
export const confidenceColor = (c: number): string =>
  c >= 0.7 ? '#22c55e' : c >= 0.55 ? '#eab308' : '#ef4444'

export const confidenceClass = (c: number): string =>
  c >= 0.7 ? 'bg-signal-green' : c >= 0.55 ? 'bg-signal-yellow' : 'bg-signal-red'

/** "2m", "3h", "Just now" — compact relative time from a Unix-ms timestamp. */
export function relTime(ms: number): string {
  const s = Math.max(0, Math.floor((Date.now() - ms) / 1000))
  if (s < 10) return 'now'
  if (s < 60) return `${s}s`
  if (s < 3600) return `${Math.floor(s / 60)}m`
  if (s < 86400) return `${Math.floor(s / 3600)}h`
  return `${Math.floor(s / 86400)}d`
}

export function fmtClock(ms: number): string {
  return new Date(ms).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' })
}
