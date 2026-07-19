import type { ReactNode } from 'react'
import type { Direction } from '../../types'

// ─── Card ─────────────────────────────────────────────────────────────────────

export function Card({ className = '', children }: { className?: string; children: ReactNode }) {
  return <div className={`mf-card ${className}`}>{children}</div>
}

// ─── Direction badge (BUY/SELL/SHORT/COVER + LONG) ────────────────────────────

export function DirectionBadge({ dir }: { dir: Direction | 'LONG' }) {
  const buy = dir === 'BUY' || dir === 'COVER' || dir === 'LONG'
  return (
    <span
      className={`mf-chip ${buy ? 'bg-signal-green/15 text-emerald-400' : 'bg-signal-red/15 text-red-400'}`}
    >
      {dir}
    </span>
  )
}

// ─── Confidence bar (value 0..1) ──────────────────────────────────────────────

export function ConfidenceBar({ value, className = '' }: { value: number; className?: string }) {
  const pct = Math.round(value * 100)
  const color = value >= 0.7 ? 'bg-signal-green' : value >= 0.55 ? 'bg-signal-yellow' : 'bg-signal-red'
  return (
    <div className={`h-1.5 w-full overflow-hidden rounded bg-base ${className}`}>
      <div className={`h-full ${color} transition-[width]`} style={{ width: `${pct}%` }} />
    </div>
  )
}

// ─── Status dot ───────────────────────────────────────────────────────────────

export function StatusDot({
  color = '#22c55e',
  pulse = false,
  size = 8,
}: {
  color?: string
  pulse?: boolean
  size?: number
}) {
  return (
    <span
      className={`inline-block rounded-full ${pulse ? 'animate-pulse-dot' : ''}`}
      style={{ width: size, height: size, background: color, boxShadow: `0 0 8px ${color}` }}
    />
  )
}

// ─── Status pill ──────────────────────────────────────────────────────────────

export function Pill({
  children,
  tone = 'neutral',
  onClick,
  title,
  className = '',
}: {
  children: ReactNode
  tone?: 'neutral' | 'green' | 'red' | 'orange' | 'yellow' | 'blue' | 'purple'
  onClick?: () => void
  title?: string
  className?: string
}) {
  const tones: Record<string, string> = {
    neutral: 'bg-surface-sunken border-line-faint text-ink-faint',
    green: 'bg-signal-green/10 border-signal-green/25 text-emerald-300',
    red: 'bg-signal-red/12 border-signal-red/30 text-red-300',
    orange: 'bg-signal-orange/12 border-signal-orange/30 text-orange-300',
    yellow: 'bg-signal-yellow/14 border-signal-yellow/40 text-yellow-300',
    blue: 'bg-signal-blue/12 border-signal-blue/30 text-blue-300',
    purple: 'bg-signal-purple/14 border-signal-purple/35 text-violet-300',
  }
  const Tag = onClick ? 'button' : 'div'
  return (
    <Tag
      onClick={onClick}
      title={title}
      className={`mf-pill border font-medium shrink-0 ${tones[tone]} ${onClick ? 'cursor-pointer' : ''} ${className}`}
    >
      {children}
    </Tag>
  )
}

// ─── Severity badge (alerts) ──────────────────────────────────────────────────

export type Severity = 'CRITICAL' | 'HIGH' | 'MEDIUM' | 'INFO'

const SEVERITY: Record<Severity, { chip: string; glow: string }> = {
  CRITICAL: { chip: 'bg-signal-red/18 text-red-300', glow: 'rgba(239,68,68,.4)' },
  HIGH: { chip: 'bg-signal-orange/18 text-orange-300', glow: 'rgba(249,115,22,.35)' },
  MEDIUM: { chip: 'bg-signal-yellow/18 text-yellow-300', glow: 'rgba(234,179,8,.3)' },
  INFO: { chip: 'bg-ink-muted/15 text-ink-muted', glow: '#334155' },
}

export function SeverityBadge({ severity }: { severity: Severity }) {
  return (
    <span className={`mf-chip ${SEVERITY[severity].chip}`}>{severity}</span>
  )
}

export const severityGlow = (s: Severity) => SEVERITY[s].glow
