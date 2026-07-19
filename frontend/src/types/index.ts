// Domain types for the Factory dashboard.

// Kept for ui/primitives.tsx compatibility (shared with Market-AI's design system).
export type Direction = 'BUY' | 'SELL' | 'SHORT' | 'COVER'

export type ProductStatus = 'DRAFT' | 'LIVE' | 'PAUSED' | 'ERROR'

export interface Product {
  id: number
  name: string
  display_name: string
  source_repo: string
  source_sha?: string
  status: ProductStatus
  port_base: number
  budget_usd: number
  dashboard_url?: string
  health_url?: string
  alpaca_key_id?: string
  adopted: boolean
  created_at: string
  updated_at: string
  /** Live metrics — populated from the product's Alpaca account (P3+). */
  metrics?: ProductMetrics
}

export interface ProductMetrics {
  today_pnl: number
  total_pnl: number
  equity: number
  /** Equity curve points for the card sparkline, oldest → newest. */
  equity_series: number[]
}

export interface ProductCheck {
  id: number
  product_id: number
  ok: boolean
  details: string
  checked_at: string
}
