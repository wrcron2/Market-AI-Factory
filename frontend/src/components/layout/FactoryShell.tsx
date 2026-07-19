import { useEffect, useState } from 'react'
import { Outlet, NavLink, useNavigate } from 'react-router-dom'
import { LayoutGrid, GitBranch, ChevronRight } from 'lucide-react'
import type { Product } from '../../types'
import { fmtSignedUSD } from '../../lib/format'
import { StatusDot } from '../ui/primitives'

const STATUS_COLOR: Record<string, string> = {
  LIVE: '#22c55e',
  PAUSED: '#eab308',
  ERROR: '#ef4444',
  DRAFT: '#6b7280',
}

/** App frame per the Factory mockup: top bar (brand + today P&L), side menu
 *  (Dashboard, Pipeline, then one entry per product), main outlet. */
export function FactoryShell() {
  const [products, setProducts] = useState<Product[]>([])
  const navigate = useNavigate()

  useEffect(() => {
    const load = async () => {
      try {
        const res = await fetch('/api/products')
        if (!res.ok) return
        const data = await res.json()
        setProducts(data.products ?? [])
      } catch {
        /* backend not up yet */
      }
    }
    load()
    const t = setInterval(load, 30_000)
    return () => clearInterval(t)
  }, [])

  const todayPnl = products.reduce((s, p) => s + (p.metrics?.today_pnl ?? 0), 0)

  return (
    <div className="flex h-screen flex-col bg-base text-ink">
      {/* Top bar */}
      <header className="flex h-[52px] shrink-0 items-center justify-between border-b border-line-faint px-5">
        <button onClick={() => navigate('/products')} className="flex items-center gap-2">
          <span className="font-mono text-[15px] font-bold tracking-wide">MARKET AI</span>
          <span className="text-ink-faint">·</span>
          <span className="font-mono text-[15px] tracking-[0.25em] text-ink-muted">FACTORY</span>
        </button>
        <div className="flex items-center gap-2 font-mono text-[13px]">
          <span className="tracking-wide text-ink-faint">TODAY P&L</span>
          <span className={todayPnl >= 0 ? 'text-emerald-400' : 'text-red-400'}>
            {fmtSignedUSD(todayPnl, 0)}
          </span>
        </div>
      </header>

      <div className="flex min-h-0 flex-1">
        {/* Side menu */}
        <aside className="mf-scroll w-[230px] shrink-0 overflow-y-auto border-r border-line-faint px-3 py-4">
          <NavItem to="/products" icon={<LayoutGrid size={15} />} label="Dashboard" />
          <NavItem to="/pipeline" icon={<GitBranch size={15} />} label="Pipeline" />

          <div className="px-2.5 pb-1.5 pt-5 text-[10px] font-semibold uppercase tracking-[0.14em] text-slate-600">
            Products
          </div>
          {products.map((p) => (
            <NavLink
              key={p.name}
              to={`/products/${p.name}`}
              className={({ isActive }) =>
                `mb-0.5 flex w-full items-center gap-2 rounded-lg px-2.5 py-1.5 font-mono text-[12.5px] transition-colors ${
                  isActive ? 'bg-signal-blue/10 text-blue-200' : 'text-slate-300 hover:bg-surface-raised'
                }`
              }
            >
              <StatusDot color={STATUS_COLOR[p.status] ?? '#6b7280'} />
              <span className="flex-1 truncate text-left">{p.name}</span>
              <ChevronRight size={12} className="text-ink-faint" />
            </NavLink>
          ))}
          {products.length === 0 && (
            <div className="px-2.5 py-1.5 text-[12px] text-ink-faint">No products yet</div>
          )}
        </aside>

        {/* Main view */}
        <main className="mf-scroll min-w-0 flex-1 overflow-y-auto p-6">
          <Outlet context={{ products }} />
        </main>
      </div>
    </div>
  )
}

function NavItem({ to, icon, label }: { to: string; icon: React.ReactNode; label: string }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `mb-0.5 flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] font-medium transition-colors ${
          isActive ? 'bg-signal-blue/10 text-blue-200' : 'text-slate-300 hover:bg-surface-raised'
        }`
      }
    >
      <span className="text-ink-faint">{icon}</span>
      {label}
    </NavLink>
  )
}
