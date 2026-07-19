import { useCallback, useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ExternalLink, LayoutDashboard, Pause, Play, Sparkles } from 'lucide-react'
import type { Product, ProductCheck } from '../types'
import { fmtUSD } from '../lib/format'
import { Card } from './ui/primitives'

/** Drill-down for one product: registry facts, monitor history, and a link
 *  out to the product's own dashboard. Grows richer in P3 (live Alpaca
 *  stats) and P5 (AI monitor reports, kill switch). */
export function ProductDetail() {
  const { name } = useParams<{ name: string }>()
  const navigate = useNavigate()
  const [product, setProduct] = useState<Product | null>(null)
  const [checks, setChecks] = useState<ProductCheck[]>([])
  const [aiReport, setAiReport] = useState<{ text: string; at: string } | null>(null)
  const [notFound, setNotFound] = useState(false)
  const [switching, setSwitching] = useState(false)

  const load = useCallback(async () => {
    if (!name) return
    try {
      const res = await fetch(`/api/products/${name}`)
      if (res.status === 404) {
        setNotFound(true)
        return
      }
      if (!res.ok) return
      const data = await res.json()
      setProduct(data.product)
      setChecks(data.checks ?? [])
      setAiReport(data.ai_report ? { text: data.ai_report, at: data.ai_report_at } : null)
    } catch {
      /* backend not up */
    }
  }, [name])

  useEffect(() => {
    load()
    const t = setInterval(load, 30_000)
    return () => clearInterval(t)
  }, [load])

  const pauseResume = async (action: 'pause' | 'resume') => {
    if (!name) return
    setSwitching(true)
    try {
      const res = await fetch(`/api/products/${name}/${action}`, { method: 'POST' })
      if (!res.ok) alert(`${action} failed: ${await res.text()}`)
      await load()
    } finally {
      setSwitching(false)
    }
  }

  if (notFound) return <div className="text-[13px] text-ink-faint">Product “{name}” not found.</div>
  if (!product) return <div className="text-[13px] text-ink-faint">Loading…</div>

  return (
    <div className="flex max-w-[900px] flex-col gap-4">
      <div className="flex items-center justify-between">
        <h1 className="font-mono text-[22px] font-semibold tracking-tight">{product.name}</h1>
        {product.status === 'LIVE' ? (
          <button onClick={() => pauseResume('pause')} disabled={switching}
            className="flex items-center gap-1.5 rounded-lg border border-yellow-500/40 bg-yellow-500/10 px-3 py-1.5 text-[12.5px] font-semibold text-yellow-300 disabled:opacity-40">
            <Pause size={13} /> Pause trading
          </button>
        ) : product.status === 'PAUSED' ? (
          <button onClick={() => pauseResume('resume')} disabled={switching}
            className="flex items-center gap-1.5 rounded-lg border border-emerald-500/40 bg-emerald-500/10 px-3 py-1.5 text-[12.5px] font-semibold text-emerald-300 disabled:opacity-40">
            <Play size={13} /> Resume
          </button>
        ) : null}
        {product.dashboard_url && (
          <button
            onClick={() => navigate(`/products/${name}/dashboard`)}
            className="flex items-center gap-1.5 rounded-lg border border-signal-blue/40 bg-signal-blue/10 px-3 py-1.5 text-[12.5px] text-blue-200 hover:bg-signal-blue/20"
          >
            <LayoutDashboard size={13} /> Open dashboard
          </button>
        )}
        {product.dashboard_url && (
          <a
            href={product.dashboard_url}
            target="_blank"
            rel="noreferrer"
            className="flex items-center gap-1.5 rounded-lg border border-line-soft bg-surface-raised px-3 py-1.5 text-[12.5px] text-ink-muted hover:text-ink"
            title="Open the product's raw URL in a new tab — usually only reachable from inside the host network"
          >
            <ExternalLink size={13} /> Direct URL
          </a>
        )}
      </div>

      <Card className="grid grid-cols-2 gap-x-8 gap-y-2 p-[18px] text-[13px] md:grid-cols-3">
        <Fact label="Status" value={product.status} />
        <Fact label="Budget" value={fmtUSD(product.budget_usd, 0)} />
        <Fact label="Source" value={product.source_repo.replace('https://github.com/', '')} />
        <Fact label="Mode" value={product.adopted ? 'Adopted (own deploy)' : `Ports ${product.port_base}+`} />
        <Fact label="Pinned SHA" value={product.source_sha ? product.source_sha.slice(0, 9) : '—'} />
        <Fact label="Alpaca key" value={product.alpaca_key_id ? `…${product.alpaca_key_id.slice(-4)}` : 'not connected'} />
      </Card>

      {aiReport && (
        <Card className="p-[18px]">
          <div className="mb-2 flex items-center gap-2 text-sm font-semibold">
            <Sparkles size={14} className="text-blue-300" /> Monitor team daily review
            <span className="ml-auto font-mono text-[11px] font-normal text-ink-faint">{aiReport.at}</span>
          </div>
          <div className="whitespace-pre-wrap text-[13px] leading-relaxed text-ink-muted">{aiReport.text}</div>
        </Card>
      )}

      <Card className="p-[18px]">
        <div className="mb-2 text-sm font-semibold">Monitor history</div>
        {checks.length === 0 && (
          <div className="text-[12.5px] text-ink-faint">
            No checks yet — the 2-hour monitor starts once the product is LIVE.
          </div>
        )}
        {checks.map((c) => (
          <div key={c.id} className="flex items-center gap-2.5 border-b border-line-faint py-1.5 text-[12.5px] last:border-0">
            <span className={`h-1.5 w-1.5 rounded-full ${c.ok ? 'bg-emerald-400' : 'bg-red-400'}`} />
            <span className="font-mono text-ink-faint">{c.checked_at}</span>
            <span className="truncate text-ink-muted">{c.ok ? 'healthy' : String(c.details)}</span>
          </div>
        ))}
      </Card>
    </div>
  )
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-[10.5px] uppercase tracking-[0.12em] text-ink-faint">{label}</div>
      <div className="mt-0.5 font-mono text-ink">{value}</div>
    </div>
  )
}
