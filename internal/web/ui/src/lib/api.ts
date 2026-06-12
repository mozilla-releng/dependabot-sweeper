// Types mirroring internal/models/models.go: PRProgress, Status, StageEvent,
// CIStatus, CheckDetail, AgentAnalysis.

export type PRStage =
  | 'pending'
  | 'analysing'
  | 'approved'
  | 'impl_starting'
  | 'impl_running'
  | 'waiting_ci'
  | 'impl_resuming'
  | 'reviewing'
  | 'finalized'
  | 'flagged_human'
  | 'gave_up'
  | 'skipped'
  | 'ci_settling'
  | 'error'

export interface StageEvent {
  stage: PRStage
  at: string     // RFC3339
  detail: string
}

// ── CI types ────────────────────────────────────────────────────────────────

export interface CheckDetail {
  name: string
  status: string
  conclusion?: string    // undefined / null for pending checks
  details_url: string
  output: string
  created_at?: string    // RFC3339
}

export interface CIStatus {
  state: string         // 'success' | 'failure' | 'pending' | ''
  total: number
  passed: number
  failed: number
  pending: number
  failures?: CheckDetail[]  // failing checks
  checks?: CheckDetail[]    // all checks
}

// ── Analysis types ──────────────────────────────────────────────────────────

export interface CodeImpact {
  file: string
  usage: string
  impact: string
}

export interface CodeChangeEntry {
  file: string
  description: string
}

export interface AgentAnalysis {
  recommendation: string  // 'approve' | 'needs_changes' | 'flag_for_human'
  confidence: string      // 'high' | 'medium' | 'low'
  review_body: string
  breaking_changes?: string[]
  deprecations?: string[]
  codebase_impact?: CodeImpact[]
  code_changes?: CodeChangeEntry[]
}

// ── PR progress ─────────────────────────────────────────────────────────────

export interface PRProgress {
  pr_number: number
  package_name: string
  bump_type: string
  stage: PRStage
  session_id?: string
  worktree_path?: string
  impl_branch?: string
  replacement_pr?: number
  last_updated: string  // RFC3339
  history: StageEvent[]
  // v2 fields (optional — absent until populated by a scan)
  old_version?: string
  new_version?: string
  ecosystem?: string
  ci?: CIStatus
  analysis?: AgentAnalysis
  budget_spent?: number
  // Terminal outcome idempotency (v3): set once a PR reaches a terminal stage.
  head_sha?: string
  outcome?: string
}

export interface Status {
  last_scan: string   // RFC3339
  next_scan: string   // RFC3339
  in_flight: number
}

// ── Workflow graph types ─────────────────────────────────────────────────────

export type NodeKind = 'entry' | 'transient' | 'active' | 'decision' | 'terminal'
export type EdgeKind = 'normal' | 'decision' | 'back'

export interface WorkflowNode {
  id: string
  kind: NodeKind
  label: string
  phase?: string
  summary: string
  detail?: string
  where?: string
}

export interface WorkflowEdge {
  from: string
  to: string
  label?: string
  kind: EdgeKind
}

export interface WorkflowGraph {
  nodes: WorkflowNode[]
  edges: WorkflowEdge[]
  entryId: string
}

export async function fetchWorkflow(): Promise<WorkflowGraph> {
  const r = await fetch('/api/v1/workflow')
  if (!r.ok) throw new Error(`/api/v1/workflow ${r.status}`)
  return r.json()
}

// ── Fetch helpers ────────────────────────────────────────────────────────────

export async function fetchPRs(): Promise<PRProgress[]> {
  const r = await fetch('/api/v1/prs')
  if (!r.ok) throw new Error(`/api/v1/prs ${r.status}`)
  return (await r.json()) ?? []
}

export async function fetchStatus(): Promise<Status> {
  const r = await fetch('/api/v1/status')
  if (!r.ok) throw new Error(`/api/v1/status ${r.status}`)
  return r.json()
}

export async function fetchLog(prNumber: number): Promise<string> {
  const r = await fetch(`/api/v1/prs/${prNumber}/log`)
  if (!r.ok) throw new Error(`/api/v1/prs/${prNumber}/log ${r.status}`)
  return r.text()
}
