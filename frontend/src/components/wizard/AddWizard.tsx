import { useCallback, useEffect, useState } from 'react'
import { useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { Check, CircleAlert, LoaderCircle, RefreshCw } from 'lucide-react'
import { Card } from '../ui/primitives'

interface StepMeta {
  id: string
  title: string
  needs_input: string[] | null
}
interface RunStep {
  step_id: string
  seq: number
  status: 'pending' | 'running' | 'ok' | 'error'
  issues: { code: string; message: string; hint?: string }[] | null
}
interface Run {
  id: number
  product_name: string
  source_repo: string
  current_step: string
  status: 'running' | 'blocked' | 'done' | 'failed'
}

const INPUT_LABELS: Record<string, { label: string; secret?: boolean; placeholder?: string }> = {
  alpaca_key_id: { label: 'Alpaca API key ID', placeholder: 'PK…' },
  alpaca_secret: { label: 'Alpaca API secret', secret: true },
  budget_usd: { label: 'Budget (USD)', placeholder: '10000' },
  dashboard_url: { label: 'Existing dashboard URL (adopted products)', placeholder: 'http://…' },
  health_url: { label: 'Existing health URL (adopted products)', placeholder: 'http://…/api/health' },
}

/** New-run form (/wizard/new) — also pre-fillable via query params, which the
 *  dedicated "Onboard Market-AI" button uses. */
export function WizardStart() {
  const [params] = useSearchParams()
  const [name, setName] = useState(params.get('name') ?? '')
  const [repo, setRepo] = useState(params.get('repo') ?? '')
  const adopted = params.get('adopted') === '1'
  const [error, setError] = useState<string | null>(null)
  const navigate = useNavigate()

  const start = async () => {
    setError(null)
    try {
      const res = await fetch('/api/wizard/runs', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ product_name: name.trim(), source_repo: repo.trim(), adopted }),
      })
      if (!res.ok) throw new Error(await res.text())
      const data = await res.json()
      navigate(`/wizard/${data.run_id}`)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'failed to start run')
    }
  }

  return (
    <div className="max-w-[560px]">
      <h1 className="text-[22px] font-semibold tracking-tight">Add product</h1>
      <p className="mb-5 mt-1 text-[13px] text-ink-faint">
        Onboard a repo as a standalone trading product{adopted && ' (adopting its existing deployment)'}.
      </p>
      <Card className="flex flex-col gap-3 p-[18px]">
        <Field label="Product name (slug)">
          <input value={name} onChange={(e) => setName(e.target.value)} placeholder="market-ai"
            className="mf-input" />
        </Field>
        <Field label="Source repo URL">
          <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="https://github.com/owner/repo"
            className="mf-input" />
        </Field>
        {error && <div className="text-[12.5px] text-red-400">{error}</div>}
        <button onClick={start} disabled={!name.trim() || !repo.trim()}
          className="mt-1 self-start rounded-lg bg-signal-blue px-4 py-2 text-[13px] font-semibold text-white disabled:opacity-40">
          Start onboarding
        </button>
      </Card>
    </div>
  )
}

/** The stepper (/wizard/:runId): hangar list on the left, current hangar's
 *  inputs + error list + Refresh/Continue on the right. */
export function WizardRun() {
  const { runId } = useParams<{ runId: string }>()
  const [meta, setMeta] = useState<StepMeta[]>([])
  const [run, setRun] = useState<Run | null>(null)
  const [steps, setSteps] = useState<RunStep[]>([])
  const [inputs, setInputs] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const navigate = useNavigate()

  const load = useCallback(async () => {
    const [m, r] = await Promise.all([
      fetch('/api/wizard/steps').then((x) => x.json()),
      fetch(`/api/wizard/runs/${runId}`).then((x) => (x.ok ? x.json() : null)),
    ])
    setMeta(m.steps ?? [])
    if (r) {
      setRun(r.run)
      setSteps(r.steps ?? [])
    }
  }, [runId])

  useEffect(() => {
    load()
  }, [load])

  const act = async (action: 'advance' | 'refresh') => {
    setBusy(true)
    try {
      const res = await fetch(`/api/wizard/runs/${runId}/${action}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(inputs),
      })
      if (res.ok) {
        const data = await res.json()
        setRun(data.run)
        setSteps(data.steps ?? [])
        setInputs({})
      }
    } finally {
      setBusy(false)
    }
  }

  if (!run) return <div className="text-[13px] text-ink-faint">Loading run…</div>

  const currentMeta = meta.find((m) => m.id === run.current_step)
  const currentStep = steps.find((s) => s.step_id === run.current_step)
  const issues = currentStep?.issues ?? []

  return (
    <div>
      <h1 className="font-mono text-[20px] font-semibold tracking-tight">
        Onboarding · {run.product_name}
      </h1>
      <p className="mb-5 mt-1 text-[12.5px] text-ink-faint">{run.source_repo}</p>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-[280px_1fr]">
        {/* Hangar list */}
        <Card className="p-3">
          {meta.map((m) => {
            const s = steps.find((x) => x.step_id === m.id)
            const state = s?.status ?? 'pending'
            const active = m.id === run.current_step && run.status !== 'done'
            return (
              <div key={m.id}
                className={`flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-[13px] ${active ? 'bg-signal-blue/10 text-blue-200' : 'text-slate-300'}`}>
                <StepIcon status={run.status === 'done' ? 'ok' : state} active={active} />
                <span className="flex-1 truncate">{m.title}</span>
              </div>
            )
          })}
        </Card>

        {/* Current hangar */}
        <Card className="p-[18px]">
          {run.status === 'done' ? (
            <div>
              <div className="mb-1 flex items-center gap-2 text-[15px] font-semibold text-emerald-400">
                <Check size={16} /> Product published
              </div>
              <p className="mb-4 text-[13px] text-ink-faint">
                {run.product_name} is LIVE and now appears on the Products dashboard.
              </p>
              <button onClick={() => navigate(`/products/${run.product_name}`)}
                className="rounded-lg bg-signal-blue px-4 py-2 text-[13px] font-semibold text-white">
                Open product
              </button>
            </div>
          ) : (
            <div>
              <div className="mb-3 text-[15px] font-semibold">{currentMeta?.title ?? run.current_step}</div>

              {(currentMeta?.needs_input ?? []).length > 0 && (
                <div className="mb-4 flex max-w-[420px] flex-col gap-3">
                  {(currentMeta?.needs_input ?? []).map((f) => {
                    const spec = INPUT_LABELS[f] ?? { label: f }
                    return (
                      <Field key={f} label={spec.label}>
                        <input
                          type={spec.secret ? 'password' : 'text'}
                          placeholder={spec.placeholder}
                          value={inputs[f] ?? ''}
                          onChange={(e) => setInputs((x) => ({ ...x, [f]: e.target.value }))}
                          className="mf-input"
                        />
                      </Field>
                    )
                  })}
                </div>
              )}

              {issues.length > 0 && (
                <div className="mb-4 flex flex-col gap-2">
                  {issues.map((i, n) => (
                    <div key={n} className="rounded-lg border border-red-500/25 bg-red-500/10 px-3 py-2 text-[12.5px]">
                      <div className="flex items-center gap-1.5 font-semibold text-red-300">
                        <CircleAlert size={13} /> {i.code}
                      </div>
                      <div className="mt-0.5 text-red-200/90">{i.message}</div>
                      {i.hint && <div className="mt-1 text-ink-faint">{i.hint}</div>}
                    </div>
                  ))}
                </div>
              )}

              <div className="flex items-center gap-2.5">
                <button onClick={() => act('advance')} disabled={busy}
                  className="flex items-center gap-2 rounded-lg bg-signal-blue px-4 py-2 text-[13px] font-semibold text-white disabled:opacity-40">
                  {busy && <LoaderCircle size={14} className="animate-spin" />} Continue
                </button>
                <button onClick={() => act('refresh')} disabled={busy}
                  className="flex items-center gap-2 rounded-lg border border-line-soft bg-surface-raised px-4 py-2 text-[13px] text-ink-muted disabled:opacity-40">
                  <RefreshCw size={13} /> Refresh
                </button>
              </div>
            </div>
          )}
        </Card>
      </div>
    </div>
  )
}

function StepIcon({ status, active }: { status: string; active: boolean }) {
  if (status === 'ok') return <Check size={14} className="text-emerald-400" />
  if (status === 'error') return <CircleAlert size={14} className="text-red-400" />
  if (active) return <LoaderCircle size={14} className="text-blue-300" />
  return <span className="h-[7px] w-[7px] rounded-full bg-slate-600" />
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-[11px] uppercase tracking-[0.1em] text-ink-faint">{label}</span>
      {children}
    </label>
  )
}
