import { useState, useEffect, useCallback, useRef } from 'react'
import { useNavigate } from 'react-router-dom'
import { RefreshCw, Play, Terminal, ExternalLink, Star, Database, Trash2, FileText, Clock, X, PackagePlus } from 'lucide-react'
import ReactMarkdown from 'react-markdown'

/** Turn "owner/Repo-Name" into a wizard-ready product slug. */
function repoSlug(fullName: string): string {
  const base = fullName.split('/').pop() ?? fullName
  return base.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 40)
}

interface RepoRow {
  id: number
  full_name: string
  url: string
  description: string | null
  stars: number
  language: string | null
  topics: string[] | null
  first_seen_at: string
  last_checked_at: string
  status: 'new' | 'good' | 'rejected' | 'researched'
  rejected_reason: string | null
  research_notion_url: string | null
  researched_at: string | null
  has_report?: boolean
}

interface AgentStatus {
  running: boolean
  last_run: string | null
  schedule?: string
}

interface PipelineStatus {
  scout: AgentStatus
  research: AgentStatus
}

// The Scout/Research agents run natively in the Go backend using the same
// LLM routing as the Ask AI panel — so the full model list is available:
// Anthropic API, Groq cloud, and local Ollama models on the host.
type AgentModel =
  | 'claude-sonnet'
  | 'deepseek-r1'
  | 'qwen3'
  | 'deepseek-r1-local'
  | 'qwen3-local'
  | 'glm-5.2'

const AGENT_MODELS: { value: AgentModel; label: string }[] = [
  { value: 'claude-sonnet',     label: 'Claude Sonnet (API)' },
  { value: 'deepseek-r1',       label: 'DeepSeek R1 (Groq cloud)' },
  { value: 'qwen3',             label: 'Qwen3 (Groq cloud)' },
  { value: 'deepseek-r1-local', label: 'DeepSeek R1 7B (Ollama · local)' },
  { value: 'qwen3-local',       label: 'Qwen3 4B (Ollama · local)' },
  { value: 'glm-5.2',           label: 'GLM-5.2 (NVIDIA cloud)' },
]

type RepoFilter = 'all' | 'new' | 'good' | 'rejected' | 'researched'

const STATUS_BADGE: Record<string, { bg: string; color: string; border: string }> = {
  new:        { bg: '#1e3a5f', color: '#60a5fa', border: '#3b82f640' },
  good:       { bg: '#14532d', color: '#22c55e', border: '#22c55e40' },
  rejected:   { bg: '#450a0a', color: '#ef4444', border: '#ef444440' },
  researched: { bg: '#3b0764', color: '#a855f7', border: '#a855f740' },
}

function fmtDate(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (isNaN(d.getTime())) return '—'
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })
}

function timeAgo(iso: string | null | undefined): string {
  if (!iso) return 'never'
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 0) return 'just now'
  const m = Math.floor(ms / 60000)
  const h = Math.floor(m / 60)
  const d = Math.floor(h / 24)
  if (d > 0) return `${d}d ago`
  if (h > 0) return `${h}h ago`
  if (m > 0) return `${m}m ago`
  return 'just now'
}

export function PipelinePanel() {
  const navigate = useNavigate()
  const [repos, setRepos] = useState<RepoRow[]>([])
  const [pipelineStatus, setPipelineStatus] = useState<PipelineStatus | null>(null)
  const [logs, setLogs] = useState<string[]>([])
  const [filter, setFilter] = useState<RepoFilter>('all')
  const [loadingRepos, setLoadingRepos] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [scoutLoading, setScoutLoading] = useState(false)
  const [researchLoading, setResearchLoading] = useState(false)
  const [scoutModel, setScoutModel] = useState<AgentModel>('claude-sonnet')
  const [researchModel, setResearchModel] = useState<AgentModel>('claude-sonnet')
  const [report, setReport] = useState<{ fullName: string; markdown: string } | null>(null)
  const [reportLoading, setReportLoading] = useState(false)
  const [researchingIds, setResearchingIds] = useState<Set<number>>(new Set())
  const logRef = useRef<HTMLPreElement>(null)

  const loadRepos = useCallback(async () => {
    try {
      const res = await fetch('/api/pipeline/repos')
      const data = await res.json()
      if (data.error) setError(data.error)
      else setError(null)
      setRepos(data.repos ?? [])
    } catch { /* keep existing */ }
    finally { setLoadingRepos(false) }
  }, [])

  const loadStatus = useCallback(async () => {
    try {
      const res = await fetch('/api/pipeline/status')
      const data: PipelineStatus = await res.json()
      setPipelineStatus(data)
    } catch { /* keep existing */ }
  }, [])

  const loadLogs = useCallback(async () => {
    try {
      const res = await fetch('/api/pipeline/logs')
      const data = await res.json()
      setLogs(data.lines ?? [])
    } catch { /* keep existing */ }
  }, [])

  const clearLogs = useCallback(async () => {
    try {
      await fetch('/api/pipeline/logs/clear', { method: 'POST' })
      setLogs([])
    } catch { /* keep existing */ }
  }, [])

  const openReport = useCallback(async (repo: RepoRow) => {
    setReportLoading(true)
    setReport({ fullName: repo.full_name, markdown: '' })
    try {
      const res = await fetch(`/api/pipeline/report/${repo.id}`)
      const data = await res.json()
      setReport({ fullName: data.full_name ?? repo.full_name, markdown: data.report ?? '' })
    } catch {
      setReport({ fullName: repo.full_name, markdown: '⚠️ Failed to load report.' })
    } finally {
      setReportLoading(false)
    }
  }, [])

  useEffect(() => {
    const refresh = () => { loadRepos(); loadStatus(); loadLogs() }
    refresh()
    const iv = setInterval(refresh, 10_000)
    return () => clearInterval(iv)
  }, [loadRepos, loadStatus, loadLogs])

  // Clear the "researching" flag on a row as soon as its report shows up
  // (or it moves off 'good', e.g. rejected on retry) via the next repo poll.
  useEffect(() => {
    setResearchingIds(prev => {
      if (prev.size === 0) return prev
      const next = new Set(prev)
      for (const repo of repos) {
        if (next.has(repo.id) && (repo.has_report || repo.status !== 'good')) next.delete(repo.id)
      }
      return next.size === prev.size ? prev : next
    })
  }, [repos])

  useEffect(() => {
    if (logRef.current) {
      logRef.current.scrollTop = logRef.current.scrollHeight
    }
  }, [logs])

  const runScout = async () => {
    setScoutLoading(true)
    try {
      const res = await fetch('/api/pipeline/run/scout', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: scoutModel }),
      })
      const data = await res.json()
      if (!data.started && data.message) {
        // Already running — message shown via button state
      }
      setTimeout(() => { loadStatus(); loadLogs() }, 800)
    } finally {
      setScoutLoading(false)
    }
  }

  const runResearch = async () => {
    setResearchLoading(true)
    try {
      const res = await fetch('/api/pipeline/run/research', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: researchModel }),
      })
      const data = await res.json()
      if (!data.started && data.message) {
        // Already running — message shown via button state
      }
      setTimeout(() => { loadStatus(); loadLogs() }, 800)
    } finally {
      setResearchLoading(false)
    }
  }

  const researchOne = useCallback((repo: RepoRow) => {
    setResearchingIds(prev => new Set(prev).add(repo.id))
    fetch('/api/pipeline/run/research', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ model: researchModel, repo_id: repo.id }),
    }).catch(() => {})
    // Safety net in case the row never flips (e.g. repeated LLM failure) —
    // the repos-effect above clears it earlier once a report lands.
    setTimeout(() => {
      setResearchingIds(prev => {
        const next = new Set(prev)
        next.delete(repo.id)
        return next
      })
    }, 120_000)
  }, [researchModel])

  const filteredRepos = filter === 'all' ? repos : repos.filter(r => r.status === filter)

  const counts = {
    all: repos.length,
    new: repos.filter(r => r.status === 'new').length,
    good: repos.filter(r => r.status === 'good').length,
    rejected: repos.filter(r => r.status === 'rejected').length,
    researched: repos.filter(r => r.status === 'researched').length,
  }

  const scout = pipelineStatus?.scout
  const research = pipelineStatus?.research

  return (
    <div style={{ padding: '0 0 40px' }}>

      {/* Header */}
      <div style={{
        display: 'flex', alignItems: 'center', justifyContent: 'space-between', marginBottom: 24,
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <Database size={18} style={{ color: '#a855f7' }} />
          <h2 style={{ margin: 0, fontSize: 16, fontWeight: 700, color: '#e2e8f0' }}>
            Repo Scout & Research Pipeline
          </h2>
          {repos.length > 0 && (
            <span style={{
              background: '#1e293b', color: '#94a3b8',
              borderRadius: 10, padding: '2px 8px', fontSize: 11, fontWeight: 600,
            }}>{repos.length} repos</span>
          )}
        </div>
        <button onClick={() => { loadRepos(); loadStatus(); loadLogs() }} style={{
          background: 'transparent', border: '1px solid #334155',
          color: '#94a3b8', borderRadius: 6, padding: '4px 10px',
          cursor: 'pointer', fontSize: 12, display: 'flex', alignItems: 'center', gap: 4,
        }}>
          <RefreshCw size={11} /> Refresh
        </button>
      </div>

      {/* ── Flow Diagram ──────────────────────────────────────────────────────── */}
      <div style={{
        background: '#0d1117', border: '1px solid #1e293b',
        borderRadius: 14, padding: '24px', marginBottom: 28, overflowX: 'auto',
      }}>
        <div style={{ display: 'flex', alignItems: 'stretch', gap: 0, minWidth: 680 }}>

          <FlowBox
            icon="🔍"
            title="Scout Agent"
            subtitle="GitHub search · dedupes · classifies"
            running={scout?.running ?? false}
            runColor="#3b82f6"
            meta={scout?.last_run ? `Last run ${timeAgo(scout.last_run)}` : 'Never run'}
            schedule={scout?.schedule}
          />
          <Arrow />

          {/* Supabase DB box */}
          <div style={{
            background: '#1e293b', borderRadius: 10, border: '1px solid #334155',
            padding: '16px 20px', flex: '1.4 1 0', display: 'flex', flexDirection: 'column', gap: 8,
          }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <span style={{ fontSize: 20 }}>🗄️</span>
              <div style={{ fontSize: 12, fontWeight: 700, color: '#e2e8f0' }}>
                github_repo_scout
              </div>
            </div>
            <div style={{ fontSize: 11, color: '#475569' }}>SQL (SQLite) · status lifecycle</div>
            <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
              {[
                { label: 'new',        count: counts.new,        color: '#60a5fa' },
                { label: 'good',       count: counts.good,       color: '#22c55e' },
                { label: 'researched', count: counts.researched, color: '#a855f7' },
                { label: 'rejected',   count: counts.rejected,   color: '#ef4444' },
              ].map(({ label, count, color }) => (
                <span key={label} style={{
                  fontSize: 10, fontWeight: 700, padding: '1px 7px',
                  borderRadius: 999, color,
                  background: color + '18', border: `1px solid ${color}30`,
                }}>
                  {count} {label}
                </span>
              ))}
            </div>
            <div style={{ fontSize: 10, color: '#475569', marginTop: 2 }}>
              new → good → researched (or rejected)
            </div>
          </div>

          <Arrow />

          <FlowBox
            icon="🧠"
            title="Research Agent"
            subtitle="Deep analysis · writes Notion page"
            running={research?.running ?? false}
            runColor="#22c55e"
            meta={research?.last_run ? `Last run ${timeAgo(research.last_run)}` : 'Never run'}
          />
          <Arrow />

          {/* Notion box */}
          <div style={{
            background: '#1e293b', borderRadius: 10, border: '1px solid #334155',
            padding: '16px 20px', minWidth: 110, display: 'flex',
            flexDirection: 'column', alignItems: 'center', gap: 6, justifyContent: 'center',
          }}>
            <span style={{ fontSize: 24 }}>📝</span>
            <div style={{ fontSize: 12, fontWeight: 700, color: '#e2e8f0' }}>Reports</div>
            <div style={{ fontSize: 11, color: '#475569', textAlign: 'center' }}>
              dashboard + Notion<br/>
              <span style={{ color: '#a855f7', fontWeight: 600 }}>{counts.researched} written</span>
            </div>
          </div>
        </div>
      </div>

      {/* ── Repo Table ────────────────────────────────────────────────────────── */}
      <div style={{
        background: '#131720', border: '1px solid #1e293b', borderRadius: 14, marginBottom: 24,
      }}>
        {/* Filter tabs */}
        <div style={{
          display: 'flex', gap: 2, padding: '12px 16px 0',
          borderBottom: '1px solid #1e293b', flexWrap: 'wrap',
        }}>
          {([
            ['all', 'All'],
            ['new', 'New'],
            ['good', 'Good'],
            ['researched', 'Researched'],
            ['rejected', 'Rejected'],
          ] as [RepoFilter, string][]).map(([key, label]) => {
            const cfg = key !== 'all' ? STATUS_BADGE[key] : null
            const isActive = filter === key
            return (
              <button key={key} onClick={() => setFilter(key)} style={{
                background: isActive ? (cfg?.bg ?? '#1e293b') : 'transparent',
                color: isActive ? (cfg?.color ?? '#e2e8f0') : '#64748b',
                border: isActive
                  ? `1px solid ${cfg?.border ?? '#334155'}`
                  : '1px solid transparent',
                borderRadius: '6px 6px 0 0', padding: '6px 14px',
                fontSize: 12, fontWeight: 600, cursor: 'pointer', transition: 'all .15s',
              }}>
                {label}
                <span style={{ marginLeft: 5, opacity: 0.7, fontSize: 10 }}>
                  {counts[key]}
                </span>
              </button>
            )
          })}
        </div>

        {/* Error banner */}
        {error && (
          <div style={{
            margin: '12px 16px', padding: '10px 14px',
            background: '#450a0a', border: '1px solid #ef444444',
            borderRadius: 8, color: '#ef4444', fontSize: 12,
          }}>
            {error}
          </div>
        )}

        {/* Table */}
        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ borderBottom: '1px solid #1e293b' }}>
                {['Repository', 'Stars', 'Language', 'Status', 'First Seen', 'Report'].map(col => (
                  <th key={col} style={{
                    padding: '10px 16px', textAlign: 'left',
                    fontSize: 11, fontWeight: 600, color: '#64748b',
                    textTransform: 'uppercase', letterSpacing: '0.05em', whiteSpace: 'nowrap',
                  }}>{col}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {loadingRepos && (
                <tr>
                  <td colSpan={6} style={{ padding: '32px 16px', textAlign: 'center', color: '#475569' }}>
                    Loading repos…
                  </td>
                </tr>
              )}
              {!loadingRepos && filteredRepos.length === 0 && !error && (
                <tr>
                  <td colSpan={6} style={{ padding: '48px 16px', textAlign: 'center', color: '#475569' }}>
                    {filter === 'all'
                      ? 'No repos yet — run the Scout Agent to start discovering repos.'
                      : `No repos with status "${filter}".`}
                  </td>
                </tr>
              )}
              {filteredRepos.map((repo, i) => {
                const cfg = STATUS_BADGE[repo.status] ?? STATUS_BADGE.new
                return (
                  <tr
                    key={repo.id}
                    style={{ borderBottom: i < filteredRepos.length - 1 ? '1px solid #1a2030' : undefined }}
                    onMouseEnter={e => (e.currentTarget.style.background = '#1a2030')}
                    onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
                  >
                    <td style={{ padding: '12px 16px', maxWidth: 300 }}>
                      <a
                        href={repo.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        style={{
                          color: '#93c5fd', fontWeight: 600, textDecoration: 'none', fontSize: 13,
                          display: 'inline-flex', alignItems: 'center', gap: 5,
                        }}
                      >
                        {repo.full_name}
                        <ExternalLink size={11} style={{ opacity: 0.5, flexShrink: 0 }} />
                      </a>
                      {repo.description && (
                        <div style={{
                          color: '#475569', fontSize: 11, marginTop: 3,
                          overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
                          maxWidth: 280,
                        }}>
                          {repo.description}
                        </div>
                      )}
                    </td>
                    <td style={{ padding: '12px 16px', color: '#94a3b8', whiteSpace: 'nowrap' }}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
                        <Star size={11} style={{ color: '#eab308', flexShrink: 0 }} />
                        {repo.stars.toLocaleString()}
                      </span>
                    </td>
                    <td style={{ padding: '12px 16px', color: '#64748b', whiteSpace: 'nowrap', fontSize: 12 }}>
                      {repo.language ?? '—'}
                    </td>
                    <td style={{ padding: '12px 16px', whiteSpace: 'nowrap' }}>
                      <span style={{
                        background: cfg.bg, color: cfg.color, border: `1px solid ${cfg.border}`,
                        borderRadius: 999, padding: '2px 10px', fontSize: 11, fontWeight: 700,
                      }}>
                        {repo.status}
                      </span>
                    </td>
                    <td style={{ padding: '12px 16px', color: '#475569', whiteSpace: 'nowrap', fontSize: 12 }}>
                      {fmtDate(repo.first_seen_at)}
                    </td>
                    <td style={{ padding: '12px 16px', whiteSpace: 'nowrap' }}>
                      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
                        {repo.has_report ? (
                          <button
                            onClick={() => openReport(repo)}
                            style={{
                              background: '#3b0764', color: '#a855f7', border: '1px solid #a855f740',
                              borderRadius: 6, padding: '3px 10px', fontSize: 11, fontWeight: 600,
                              cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 4,
                            }}
                          >
                            <FileText size={11} /> View
                          </button>
                        ) : repo.status === 'good' ? (
                          researchingIds.has(repo.id) ? (
                            <span style={{
                              display: 'inline-flex', alignItems: 'center', gap: 6,
                              color: '#64748b', fontSize: 11, fontWeight: 600,
                            }}>
                              <RunningDot running color="#38bdf8" /> Researching…
                            </span>
                          ) : (
                            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 10 }}>
                              <span style={{
                                display: 'inline-flex', alignItems: 'center', gap: 4,
                                color: '#64748b', fontSize: 11, fontWeight: 600,
                              }}>
                                <Clock size={11} /> Pending research
                              </span>
                              <button
                                onClick={() => researchOne(repo)}
                                style={{
                                  background: '#082f49', color: '#38bdf8', border: '1px solid #38bdf840',
                                  borderRadius: 6, padding: '3px 10px', fontSize: 11, fontWeight: 600,
                                  cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 4,
                                }}
                              >
                                <Play size={11} /> Research
                              </button>
                            </span>
                          )
                        ) : (
                          <span style={{ color: '#334155', fontSize: 12 }}>—</span>
                        )}
                        {repo.research_notion_url && (
                          <a
                            href={repo.research_notion_url}
                            target="_blank"
                            rel="noopener noreferrer"
                            style={{
                              color: '#a855f7', fontSize: 12, textDecoration: 'none',
                              display: 'inline-flex', alignItems: 'center', gap: 4,
                            }}
                          >
                            Notion <ExternalLink size={10} />
                          </a>
                        )}
                        {(repo.status === 'good' || repo.status === 'researched') && (
                          <button
                            onClick={() =>
                              navigate(`/wizard/new?repo=${encodeURIComponent(`https://github.com/${repo.full_name}`)}&name=${repoSlug(repo.full_name)}`)
                            }
                            title="Onboard this repo as a trading product"
                            style={{
                              background: '#052e16', color: '#4ade80', border: '1px solid #4ade8040',
                              borderRadius: 6, padding: '3px 10px', fontSize: 11, fontWeight: 600,
                              cursor: 'pointer', display: 'inline-flex', alignItems: 'center', gap: 4,
                            }}
                          >
                            <PackagePlus size={11} /> Add
                          </button>
                        )}
                      </span>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      </div>

      {/* ── Run Controls ─────────────────────────────────────────────────────── */}
      <div style={{
        display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))',
        gap: 16, marginBottom: 24,
      }}>
        {/* Scout */}
        <RunControl
          icon="🔍"
          title="Scout Agent"
          description="Searches GitHub for trading/quant repos, dedupes against the github_repo_scout SQL table, and classifies each new repo as good or rejected using the selected model."
          running={scout?.running ?? false}
          loading={scoutLoading}
          onRun={runScout}
          btnColor="#3b82f6"
          btnTextColor="#fff"
          lastRun={scout?.last_run}
          extra={`Schedule: ${scout?.schedule ?? 'cron 6h'}`}
          model={scoutModel}
          onModelChange={setScoutModel}
        />

        {/* Research */}
        <RunControl
          icon="🧠"
          title="Research Agent"
          description={<>Picks up <em style={{ color: '#22c55e' }}>good</em> repos, deep-analyses their README + metadata, saves a report you can open right here (and to Notion when configured), then marks them <em style={{ color: '#a855f7' }}>researched</em>.</>}
          running={research?.running ?? false}
          loading={researchLoading}
          onRun={runResearch}
          btnColor="#22c55e"
          btnTextColor="#000"
          lastRun={research?.last_run}
          extra="Triggers on: status='good' AND not yet researched"
          model={researchModel}
          onModelChange={setResearchModel}
        />
      </div>

      {/* ── Log Output ───────────────────────────────────────────────────────── */}
      <div style={{
        background: '#0d1117', border: '1px solid #1e293b', borderRadius: 12, overflow: 'hidden',
      }}>
        <div style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          padding: '10px 16px', borderBottom: '1px solid #1e293b',
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <Terminal size={14} style={{ color: '#64748b' }} />
            <span style={{ fontSize: 12, fontWeight: 600, color: '#94a3b8' }}>
              logs/scout.log — last 50 lines
            </span>
          </div>
          <div style={{ display: 'flex', gap: 6 }}>
            <button onClick={loadLogs} style={{
              background: 'transparent', border: '1px solid #1e293b',
              color: '#475569', borderRadius: 4, padding: '2px 8px',
              cursor: 'pointer', fontSize: 11, display: 'flex', alignItems: 'center', gap: 3,
            }}>
              <RefreshCw size={10} /> Refresh
            </button>
            <button onClick={clearLogs} style={{
              background: 'transparent', border: '1px solid #1e293b',
              color: '#475569', borderRadius: 4, padding: '2px 8px',
              cursor: 'pointer', fontSize: 11, display: 'flex', alignItems: 'center', gap: 3,
            }}>
              <Trash2 size={10} /> Clear
            </button>
          </div>
        </div>
        <pre ref={logRef} style={{
          margin: 0, padding: '14px 16px', fontSize: 11, lineHeight: 1.7,
          fontFamily: 'Monaco, Menlo, Consolas, monospace',
          maxHeight: 280, overflowY: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-all',
        }}>
          {logs.length === 0 ? (
            <span style={{ color: '#334155' }}>
              No logs yet — run the scout pipeline to see output here.
            </span>
          ) : logs.map((line, i) => {
            const isError = /error|fail/i.test(line)
            const isSuccess = /complete|done|success/i.test(line)
            const isSection = line.startsWith('===')
            const color = isError ? '#ef4444' : isSuccess ? '#22c55e' : isSection ? '#a855f7' : '#64748b'
            return (
              <span key={i} style={{ display: 'block', color }}>
                {line}
              </span>
            )
          })}
        </pre>
      </div>

      {/* ── Research Report Modal ────────────────────────────────────────────── */}
      {report && (
        <div
          onClick={() => setReport(null)}
          style={{
            position: 'fixed', inset: 0, zIndex: 100,
            background: 'rgba(0,0,0,0.65)', display: 'flex',
            alignItems: 'center', justifyContent: 'center', padding: 24,
          }}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            style={{
              background: '#0d1117', border: '1px solid #334155', borderRadius: 14,
              width: 'min(760px, 100%)', maxHeight: '85vh', display: 'flex',
              flexDirection: 'column', overflow: 'hidden',
            }}
          >
            <div style={{
              display: 'flex', alignItems: 'center', justifyContent: 'space-between',
              padding: '14px 20px', borderBottom: '1px solid #1e293b',
            }}>
              <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <FileText size={15} style={{ color: '#a855f7' }} />
                <span style={{ fontSize: 14, fontWeight: 700, color: '#e2e8f0' }}>
                  {report.fullName}
                </span>
              </div>
              <button
                onClick={() => setReport(null)}
                style={{
                  background: 'transparent', border: '1px solid #334155', color: '#94a3b8',
                  borderRadius: 6, padding: '4px 8px', cursor: 'pointer',
                  display: 'flex', alignItems: 'center',
                }}
              >
                <X size={14} />
              </button>
            </div>
            <div className="prose-mf" style={{
              padding: '18px 24px', overflowY: 'auto', color: '#cbd5e1',
              fontSize: 13, lineHeight: 1.7,
            }}>
              {reportLoading ? (
                <span style={{ color: '#475569' }}>Loading report…</span>
              ) : report.markdown ? (
                <ReactMarkdown>{report.markdown}</ReactMarkdown>
              ) : (
                <span style={{ color: '#475569' }}>No report stored for this repo yet.</span>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  )
}

// ── Sub-components ────────────────────────────────────────────────────────────

interface FlowBoxProps {
  icon: string
  title: string
  subtitle: string
  running: boolean
  runColor: string
  meta: string
  schedule?: string
}

function FlowBox({ icon, title, subtitle, running, runColor, meta, schedule }: FlowBoxProps) {
  return (
    <div style={{
      background: '#1e293b', borderRadius: 10,
      border: `1px solid ${running ? runColor + '66' : '#334155'}`,
      padding: '16px 20px', flex: '1 1 0', minWidth: 160,
      boxShadow: running ? `0 0 12px ${runColor}22` : 'none',
      transition: 'border-color .3s, box-shadow .3s',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
        <span style={{ fontSize: 20 }}>{icon}</span>
        <div style={{ flex: 1 }}>
          <div style={{ fontSize: 12, fontWeight: 700, color: '#e2e8f0' }}>{title}</div>
        </div>
        <RunningDot running={running} color={runColor} />
      </div>
      <div style={{ fontSize: 11, color: '#475569', marginBottom: 8 }}>{subtitle}</div>
      <div style={{ fontSize: 10, color: '#334155' }}>{meta}</div>
      {schedule && <div style={{ fontSize: 10, color: '#475569', marginTop: 2 }}>{schedule}</div>}
    </div>
  )
}

interface RunControlProps {
  icon: string
  title: string
  description: React.ReactNode
  running: boolean
  loading: boolean
  onRun: () => void
  btnColor: string
  btnTextColor: string
  lastRun?: string | null
  extra?: string
  model: AgentModel
  onModelChange: (m: AgentModel) => void
}

function RunControl({
  icon, title, description, running, loading, onRun,
  btnColor, btnTextColor, lastRun, extra, model, onModelChange,
}: RunControlProps) {
  const busy = loading || running
  return (
    <div style={{
      background: '#131720', border: '1px solid #1e293b', borderRadius: 12, padding: '20px',
    }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 10 }}>
        <span style={{ fontSize: 18 }}>{icon}</span>
        <span style={{ fontWeight: 700, fontSize: 14, color: '#e2e8f0' }}>{title}</span>
        <RunningDot running={running} color={btnColor} />
      </div>
      <p style={{ color: '#475569', fontSize: 12, margin: '0 0 12px', lineHeight: 1.6 }}>
        {description}
      </p>
      <div style={{ marginBottom: 14 }}>
        <div style={{ fontSize: 10, fontWeight: 600, color: '#64748b', marginBottom: 4, textTransform: 'uppercase', letterSpacing: '0.05em' }}>Model</div>
        <select
          value={model}
          onChange={(e) => onModelChange(e.target.value as AgentModel)}
          disabled={busy}
          style={{
            background: '#1e293b', color: '#e2e8f0', border: '1px solid #334155',
            borderRadius: 8, padding: '6px 10px', fontSize: 12, width: '100%',
            cursor: busy ? 'not-allowed' : 'pointer', outline: 'none',
          }}
        >
          {AGENT_MODELS.map((m) => (
            <option key={m.value} value={m.value}>{m.label}</option>
          ))}
        </select>
      </div>
      <button
        onClick={onRun}
        disabled={busy}
        style={{
          background: busy ? '#1e293b' : btnColor,
          color: busy ? '#64748b' : btnTextColor,
          border: busy ? '1px solid #334155' : 'none',
          borderRadius: 8, padding: '8px 16px',
          fontSize: 13, fontWeight: 600, cursor: busy ? 'not-allowed' : 'pointer',
          display: 'inline-flex', alignItems: 'center', gap: 6,
          transition: 'all .15s',
        }}
      >
        <Play size={13} />
        {running ? 'Running…' : loading ? 'Starting…' : `Run ${title} Now`}
      </button>
      <div style={{ marginTop: 10, color: '#475569', fontSize: 11 }}>
        Last run: {timeAgo(lastRun)}
        {extra && <span style={{ marginLeft: 8, color: '#334155' }}>· {extra}</span>}
      </div>
    </div>
  )
}

function Arrow() {
  return (
    <div style={{ display: 'flex', alignItems: 'center', padding: '0 6px', flexShrink: 0 }}>
      <svg width="28" height="14" viewBox="0 0 28 14" fill="none">
        <path
          d="M1 7 H22 M17 2 L26 7 L17 12"
          stroke="#334155"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </div>
  )
}

function RunningDot({ running, color = '#22c55e' }: { running: boolean; color?: string }) {
  return (
    <span
      title={running ? 'Running' : 'Idle'}
      style={{
        display: 'inline-block', width: 7, height: 7, borderRadius: '50%',
        background: running ? color : '#334155', flexShrink: 0,
        animation: running ? 'pulse 1.5s ease-in-out infinite' : 'none',
      }}
    />
  )
}
