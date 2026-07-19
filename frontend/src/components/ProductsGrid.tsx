import { useNavigate, useOutletContext } from 'react-router-dom'
import type { Product } from '../types'
import { fmtSignedUSD, fmtUSD } from '../lib/format'
import { Card } from './ui/primitives'

const STATUS_STYLE: Record<string, { dot: string; text: string; label: string }> = {
  LIVE: { dot: 'bg-emerald-400', text: 'text-emerald-400', label: 'LIVE' },
  PAUSED: { dot: 'bg-yellow-400', text: 'text-yellow-400', label: 'PAUSED' },
  ERROR: { dot: 'bg-red-400', text: 'text-red-400', label: 'ERROR' },
  DRAFT: { dot: 'bg-slate-500', text: 'text-slate-400', label: 'DRAFT' },
}

/** Main view per the mockup: one card per product — today/total P&L, equity
 *  sparkline, allocated budget bar, status badge. Click → drill-down. */
export function ProductsGrid() {
  const { products } = useOutletContext<{ products: Product[] }>()
  const navigate = useNavigate()

  const hasMarketAI = products.some((p) => p.name === 'market-ai')

  return (
    <div>
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-[22px] font-semibold tracking-tight">Products</h1>
          <p className="mb-5 mt-1 text-[13px] text-ink-faint">
            Live trading agents auto-buying and selling via their own connected Alpaca accounts.
          </p>
        </div>
        <div className="flex gap-2">
          {!hasMarketAI && (
            <button
              onClick={() =>
                navigate('/wizard/new?name=market-ai&repo=https://github.com/wrcron2/Market-AI&adopted=1')
              }
              className="rounded-lg border border-emerald-500/40 bg-emerald-500/10 px-3.5 py-2 text-[12.5px] font-semibold text-emerald-300"
            >
              Onboard Market-AI
            </button>
          )}
          <button
            onClick={() => navigate('/wizard/new')}
            className="rounded-lg bg-signal-blue px-3.5 py-2 text-[12.5px] font-semibold text-white"
          >
            ＋ Add product
          </button>
        </div>
      </div>

      {products.length === 0 && (
        <Card className="p-8 text-center text-[13px] text-ink-faint">
          No products yet. Approve a researched repo in the Pipeline and press <b>Add</b> to
          onboard the first one.
        </Card>
      )}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
        {products.map((p) => (
          <ProductCard key={p.name} product={p} onClick={() => navigate(`/products/${p.name}`)} />
        ))}
      </div>
    </div>
  )
}

function ProductCard({ product: p, onClick }: { product: Product; onClick: () => void }) {
  const s = STATUS_STYLE[p.status] ?? STATUS_STYLE.DRAFT
  const m = p.metrics
  const today = m?.today_pnl ?? 0
  const total = m?.total_pnl ?? 0
  const series = m?.equity_series ?? []
  const up = series.length >= 2 ? series[series.length - 1] >= series[0] : today >= 0

  return (
    <Card
      className="cursor-pointer p-[18px] transition-colors hover:border-line-soft"
      // Card is a div — make it behave like a button for a11y
      // eslint-disable-next-line
      {...{ onClick, role: 'button', tabIndex: 0, onKeyDown: (e: React.KeyboardEvent) => e.key === 'Enter' && onClick() }}
    >
      <div className="flex items-center justify-between">
        <span className="font-mono text-[15px] font-semibold">{p.name}</span>
        <span className={`flex items-center gap-1.5 font-mono text-[11px] ${s.text}`}>
          <span className={`h-1.5 w-1.5 rounded-full ${s.dot}`} />
          {s.label}
        </span>
      </div>

      <div className={`mt-3 font-mono text-[28px] font-bold ${today >= 0 ? 'text-emerald-400' : 'text-red-400'}`}>
        {m ? fmtSignedUSD(today, 0) : '—'}
      </div>
      <div className="mt-0.5 text-[12px] text-ink-faint">
        Today · Total{' '}
        <span className={total >= 0 ? 'text-emerald-500' : 'text-red-400'}>
          {m ? fmtSignedUSD(total, 0) : '—'}
        </span>
      </div>

      <Sparkline series={series} up={up} />

      <BudgetBar allocated={p.budget_usd} equity={m?.equity} />
      <div className="mt-1.5 font-mono text-[12px] text-ink-faint">
        {fmtUSD(p.budget_usd, 0)} allocated
      </div>
    </Card>
  )
}

/** Inline SVG equity sparkline — no chart lib needed for a card-sized curve. */
function Sparkline({ series, up }: { series: number[]; up: boolean }) {
  const W = 280
  const H = 64
  if (series.length < 2) {
    return <div className="mt-4 h-[64px] rounded bg-surface-sunken/50" />
  }
  const min = Math.min(...series)
  const max = Math.max(...series)
  const span = max - min || 1
  const pts = series.map((v, i) => {
    const x = (i / (series.length - 1)) * W
    const y = H - 4 - ((v - min) / span) * (H - 8)
    return `${x.toFixed(1)},${y.toFixed(1)}`
  })
  const stroke = up ? '#34d399' : '#f87171'
  const fill = up ? '#34d39918' : '#f8717118'
  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="mt-4 h-[64px] w-full" preserveAspectRatio="none">
      <polygon points={`0,${H} ${pts.join(' ')} ${W},${H}`} fill={fill} />
      <polyline points={pts.join(' ')} fill="none" stroke={stroke} strokeWidth="1.8" />
    </svg>
  )
}

/** Allocation bar: how much of the budget is currently deployed as equity. */
function BudgetBar({ allocated, equity }: { allocated: number; equity?: number }) {
  const pct = allocated > 0 && equity !== undefined ? Math.min(100, (equity / allocated) * 100) : 0
  return (
    <div className="mt-3 h-[5px] w-full overflow-hidden rounded bg-surface-sunken">
      <div className="h-full rounded bg-emerald-400/80" style={{ width: `${pct}%` }} />
    </div>
  )
}
