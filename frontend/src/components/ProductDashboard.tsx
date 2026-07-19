import { useEffect, useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft, ExternalLink, RefreshCw } from 'lucide-react'
import type { Product } from '../types'

/**
 * ProductDashboard mounts a product's own web UI inside the Factory's shell
 * via the backend reverse-proxy at /api/products/<name>/proxy/*. This is
 * Plan B for the OCI-Security-List-scaling problem: a human opens
 *   /products/<name>/dashboard
 * in a browser, the iframe loads
 *   /api/products/<name>/proxy/
 * which the Factory's backend container resolves to the product's
 * internal container URL over factory-net (no published host port, no
 * per-product cloud firewall rule). Works for any product, adopted or
 * new — the backend's ServeProxy picks dashboard_url (or health_url)
 * from the registry and routes accordingly.
 *
 * The src the iframe sees is same-origin with the Factory's own port-
 * 9000 origin, so cookies and CSP behave. CSP is dropped by the proxy
 * (ModifyResponse) so the product's own framing rules don't reject the
 * iframe; the Factory's own CSP governs top-level embedding.
 *
 * Pause/ERROR/DRAFT products are blocked at the backend (503) — the
 * iframe loads that body and the human sees an actionable message,
 * rather than an opaque 502.
 */
export function ProductDashboard() {
  const { name } = useParams<{ name: string }>()
  const navigate = useNavigate()
  const [product, setProduct] = useState<Product | null>(null)
  const [iframeKey, setIframeKey] = useState(0)
  const [loadError, setLoadError] = useState(false)

  const proxyURL = name ? `/api/products/${name}/proxy/` : ''

  useEffect(() => {
    if (!name) return
    let cancelled = false
    const load = async () => {
      try {
        const res = await fetch(`/api/products/${name}`)
        if (!res.ok) return
        const data = await res.json()
        if (!cancelled) setProduct(data.product)
      } catch {
        /* backend not up */
      }
    }
    load()
    return () => { cancelled = true }
  }, [name])

  // The iframe can't easily report upstream failures to us (CORS, no
  // messaging contract), so we render an explicit "Open in new tab"
  // fallback below for the rare case the proxy returns an error code —
  // the human can see it directly and the URL is shareable.
  const handleIFrameError = () => setLoadError(true)

  return (
    <div className="flex h-screen flex-col bg-base text-ink">
      {/* Slim toolbar — back link, product name, refresh, open-raw.
          Owns its own top bar since this route lives outside FactoryShell
          (the iframe needs the full viewport for the product's SPA). */}
      <div className="flex h-[52px] shrink-0 items-center justify-between border-b border-line-faint px-4">
        <div className="flex items-center gap-3">
          <button
            onClick={() => navigate(`/products/${name}`)}
            className="flex items-center gap-1.5 rounded-lg px-2 py-1 text-[12.5px] text-ink-muted hover:bg-surface-raised hover:text-ink"
          >
            <ArrowLeft size={13} /> Back
          </button>
          <span className="font-mono text-[14px] font-semibold">{name}</span>
          {product && (
            <span className="font-mono text-[11px] text-ink-faint">
              via Factory proxy · no firewall rule required
            </span>
          )}
        </div>
        <div className="flex items-center gap-1.5">
          <button
            onClick={() => {
              setLoadError(false)
              setIframeKey((k) => k + 1) // remount the iframe to force reload
            }}
            className="flex items-center gap-1.5 rounded-lg border border-line-soft bg-surface-raised px-2.5 py-1 text-[12px] text-ink-muted hover:text-ink"
            title="Reload dashboard"
          >
            <RefreshCw size={12} /> Reload
          </button>
          {product?.dashboard_url && (
            <a
              href={product.dashboard_url}
              target="_blank"
              rel="noreferrer"
              className="flex items-center gap-1.5 rounded-lg border border-line-soft bg-surface-raised px-2.5 py-1 text-[12px] text-ink-muted hover:text-ink"
              title="Open the product's raw URL in a new tab (likely only works inside the host network)"
            >
              <ExternalLink size={12} /> Direct URL
            </a>
          )}
        </div>
      </div>

      {/* Iframe host — fills remaining vertical space. iframe key=0
          first render; reload bumps it to force a fresh load. */}
      <div className="relative flex-1 bg-surface-sunken">
        {loadError && (
          <div className="absolute inset-0 z-10 flex items-center justify-center p-8 text-center">
            <div className="max-w-md">
              <div className="text-[15px] font-semibold text-ink-muted">Dashboard unreachable</div>
              <p className="mt-2 text-[13px] text-ink-faint">
                The product's dashboard couldn't load through the proxy. The product may be
                down, its container unreachable, or the dashboard URL is misconfigured.
                Reload to retry, or check the product detail page.
              </p>
            </div>
          </div>
        )}
        <iframe
          key={iframeKey}
          src={proxyURL}
          title={`${name} dashboard`}
          className="h-full w-full border-0 bg-white"
          // onError on iframes is unreliable in most browsers (only
          // fires for the host document failing to load the src URL —
          // not for upstream 502 inside), but wiring it costs nothing
          // and helps the rare cross-origin case.
          onError={handleIFrameError}
        // Security model: the iframe loads same-origin with the Factory's
        // own /api/* routes (the proxy serves under /api/products/...).
        // We do NOT sandbox without allow-same-origin because the product's
        // SPA needs cookies + same-origin fetch to call its own backend
        // through the proxy — sandboxing would break every dashboard.
        // The trust boundary is the operator's LIVE-only allowlist: only
        // products the operator explicitly deployed and marked LIVE are
        // proxied. A malicious product's JS could reach /api/killall —
        // acceptable for a single-operator tool where the operator chose
        // to deploy that product. Multi-tenant isolation would require
        // serving the proxy from a separate origin (alt port) so the
        // product's JS gets a cross-origin context and CORS gates it.
        // No sandbox: the product's own scripts need to run for its
        // SPA to boot. allow-same-origin is implicit (no sandbox attr)
        // so cookies and fetch work through the proxy.
        allow="clipboard-read; clipboard-write; fullscreen"
        />
      </div>
    </div>
  )
}
