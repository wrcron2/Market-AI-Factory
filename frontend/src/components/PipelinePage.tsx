import { Card } from './ui/primitives'

/** Placeholder — the scout/research pipeline migrates here from Market-AI in
 *  P4, gaining a per-repo "Add" button that launches the onboarding wizard. */
export function PipelinePage() {
  return (
    <div>
      <h1 className="text-[22px] font-semibold tracking-tight">Pipeline</h1>
      <p className="mb-5 mt-1 text-[13px] text-ink-faint">
        Scout → research → approve. Approved repos become products via the Add wizard.
      </p>
      <Card className="p-8 text-center text-[13px] text-ink-faint">
        The scout &amp; research pipeline migrates here from Market-AI (phase P4). Until then it
        keeps running in the Market-AI dashboard's Pipeline tab.
      </Card>
    </div>
  )
}
