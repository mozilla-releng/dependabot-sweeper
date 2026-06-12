// Stage-to-phase mapping, colours, and helpers.
// Single source of truth imported by StageBadge, PipelineBoard, Column,
// StateMap, and PrCard — keeps all visual representations consistent.

import type { PRStage } from './api'

// ── Phase definitions ──────────────────────────────────────────────────────

/** The 5 Kanban phases shown in the pipeline board. */
export type Phase = 'Queued' | 'Analysing' | 'Implementing' | 'CI+Review' | 'Done+Flagged'

/** Ordered phase list (left → right in the board). */
export const PHASES: Phase[] = [
  'Queued',
  'Analysing',
  'Implementing',
  'CI+Review',
  'Done+Flagged',
]

/** Which stages belong to each phase. */
export const PHASE_STAGES: Record<Phase, PRStage[]> = {
  'Queued':        ['pending', 'ci_settling'],
  'Analysing':     ['analysing'],
  'Implementing':  ['impl_starting', 'impl_running', 'impl_resuming'],
  'CI+Review':     ['waiting_ci', 'reviewing'],
  'Done+Flagged':  ['approved', 'finalized', 'skipped', 'flagged_human', 'gave_up', 'error'],
}

/** Reverse map: stage → phase (computed once). */
export const STAGE_PHASE: Record<PRStage, Phase> = Object.fromEntries(
  (Object.entries(PHASE_STAGES) as [Phase, PRStage[]][]).flatMap(
    ([phase, stages]) => stages.map(s => [s, phase])
  )
) as Record<PRStage, Phase>

// ── Colours ────────────────────────────────────────────────────────────────

/**
 * Tailwind CSS classes per stage — used by StageBadge and StateMap.
 * Kept here so the badge and graph always agree.
 */
export const STAGE_COLOURS: Record<string, string> = {
  pending:       'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300',
  skipped:       'bg-gray-100 text-gray-500 dark:bg-gray-700 dark:text-gray-400',
  ci_settling:   'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  analysing:     'bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-300',
  reviewing:     'bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-300',
  impl_starting: 'bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-300',
  impl_running:  'bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-300',
  impl_resuming: 'bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-300',
  waiting_ci:    'bg-orange-100 text-orange-800 dark:bg-orange-900/40 dark:text-orange-300',
  approved:      'bg-green-100 text-green-800 dark:bg-green-900/40 dark:text-green-300',
  finalized:     'bg-green-100 text-green-800 dark:bg-green-900/40 dark:text-green-300',
  flagged_human: 'bg-red-100 text-red-800 dark:bg-red-900/40 dark:text-red-300',
  gave_up:       'bg-red-200 text-red-900 dark:bg-red-900/60 dark:text-red-200',
  error:         'bg-red-200 text-red-900 dark:bg-red-900/60 dark:text-red-200',
}

/** Fallback badge colour for unknown stages. */
export const DEFAULT_COLOUR = 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-300'

/** SVG fill colour per stage — used in StateMap nodes. */
export const STAGE_FILL: Record<string, string> = {
  pending:       '#e5e7eb',
  ci_settling:   '#fde68a',
  analysing:     '#bfdbfe',
  impl_starting: '#fed7aa',
  impl_running:  '#fed7aa',
  impl_resuming: '#fed7aa',
  waiting_ci:    '#fef3c7',
  reviewing:     '#bfdbfe',
  approved:      '#bbf7d0',
  finalized:     '#bbf7d0',
  skipped:       '#f3f4f6',
  flagged_human: '#fecaca',
  gave_up:       '#fca5a5',
  error:         '#fca5a5',
}

// ── Progress ───────────────────────────────────────────────────────────────

/** Linear stage order for the progress bar (0 = start, 1 = done). */
const PROGRESS_ORDER: PRStage[] = [
  'pending', 'ci_settling',
  'analysing',
  'impl_starting', 'impl_running', 'impl_resuming', 'waiting_ci',
  'reviewing',
  'approved', 'finalized',
]

/**
 * Returns a 0..1 progress value for the compact card progress bar.
 * Terminal states (skipped/flagged/error/gave_up) return 1 since they
 * are resolved outcomes; unknown stages return 0.
 */
export function stageProgress(stage: PRStage): number {
  const terminal = new Set<PRStage>(['skipped', 'flagged_human', 'gave_up', 'error'])
  if (terminal.has(stage)) return 1
  const i = PROGRESS_ORDER.indexOf(stage)
  if (i < 0) return 0
  return i / (PROGRESS_ORDER.length - 1)
}

/**
 * Returns a human-readable label for a stage used in the state map.
 * Falls back to the raw stage value.
 */
export function stageLabel(stage: string): string {
  const labels: Record<string, string> = {
    pending:       'Pending',
    ci_settling:   'CI settling',
    analysing:     'Analysing',
    impl_starting: 'Impl start',
    impl_running:  'Impl running',
    impl_resuming: 'Impl resume',
    waiting_ci:    'Waiting CI',
    reviewing:     'Reviewing',
    approved:      'Approved',
    finalized:     'Finalized',
    skipped:       'Skipped',
    flagged_human: 'Flagged',
    gave_up:       'Gave up',
    error:         'Error',
  }
  return labels[stage] ?? stage
}
