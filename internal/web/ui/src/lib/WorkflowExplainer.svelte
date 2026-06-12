<script lang="ts">
  import { onMount } from 'svelte'
  import { fetchWorkflow } from './api'
  import type { WorkflowGraph, WorkflowNode, WorkflowEdge } from './api'
  import { STAGE_FILL } from './stages'

  // ── Data ───────────────────────────────────────────────────────────────────

  let graph = $state<WorkflowGraph | null>(null)
  let loading = $state(true)
  let error = $state('')
  let selected = $state<WorkflowNode | null>(null)

  onMount(async () => {
    try {
      graph = await fetchWorkflow()
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  })

  // ── Layout ─────────────────────────────────────────────────────────────────
  // The spec owns the graph structure; this component owns the visual positions.
  // Stage nodes are rounded rects; decision nodes are diamonds (rotated squares).

  const SVG_W = 1220, SVG_H = 248
  const NW = 80, NH = 26   // stage node width/height
  const DW = 44, DH = 44   // decision diamond bounding box
  const RX = 6             // corner radius

  // Phase-colour for decision diamond borders (same palette as stages)
  const DECISION_FILL = '#f3f4f6'  // light gray
  const DECISION_STROKE = '#6b7280'

  // Position map: node id → { x, y, w, h }
  const POS: Record<string, { x: number; y: number; w: number; h: number }> = {
    // ── Top row (y=14) — early / simple terminals ─────────────────────────
    'skipped':               { x:109, y:14,  w:NW, h:NH },
    'error':                 { x:289, y:14,  w:NW, h:NH },
    'approved':              { x:399, y:14,  w:NW, h:NH },
    'finalized':             { x:1085,y:14,  w:NW, h:NH },

    // ── Main row (y=100) — decisions and processing stages ────────────────
    'pending':               { x:10,  y:100, w:NW, h:NH },
    'dec_early_exit':        { x:109, y:89,  w:DW, h:DH },
    'dec_ci_settled':        { x:204, y:89,  w:DW, h:DH },
    'analysing':             { x:289, y:100, w:NW, h:NH },
    'dec_analysis_routing':  { x:399, y:89,  w:DW, h:DH },
    'dec_replacement_exists':{ x:494, y:89,  w:DW, h:DH },
    'impl_starting':         { x:584, y:100, w:NW, h:NH },
    'impl_running':          { x:684, y:100, w:NW, h:NH },
    'dec_ci_gate':           { x:784, y:89,  w:DW, h:DH },
    'dec_review_gate':       { x:884, y:89,  w:DW, h:DH },

    // ── Bottom row (y=188) — loop-back stages and late terminals ──────────
    'ci_settling':           { x:204, y:188, w:NW, h:NH },
    'impl_resuming':         { x:684, y:188, w:NW, h:NH },
    'waiting_ci':            { x:784, y:188, w:NW, h:NH },
    'gave_up':               { x:884, y:188, w:NW, h:NH },
    'flagged_human':         { x:1085,y:188, w:NW, h:NH },
  }

  // ── Helpers ────────────────────────────────────────────────────────────────

  function box(id: string) { return POS[id] }

  /** Centre of a node's bounding box. */
  function cx(id: string) { const b = box(id); return b.x + b.w / 2 }
  function cy(id: string) { const b = box(id); return b.y + b.h / 2 }

  /** Right edge (horizontal mid). */
  function right(id: string) { const b = box(id); return b.x + b.w }
  /** Left edge. */
  function left(id: string) { return box(id).x }
  /** Top edge. */
  function top(id: string) { return box(id).y }
  /** Bottom edge. */
  function bottom(id: string) { const b = box(id); return b.y + b.h }

  /** Diamond points as an SVG polygon string. */
  function diamondPoints(id: string): string {
    const b = box(id)
    const mx = b.x + b.w / 2, my = b.y + b.h / 2
    return `${mx},${b.y} ${b.x + b.w},${my} ${mx},${b.y + b.h} ${b.x},${my}`
  }

  /** Fill for a stage node. Decision nodes use DECISION_FILL. */
  function nodeFill(n: WorkflowNode): string {
    if (n.kind === 'decision') return DECISION_FILL
    return STAGE_FILL[n.id] ?? '#e5e7eb'
  }

  /**
   * Compute an SVG path for an edge.
   * Strategy: simple cubic bezier, with special routing for long/back edges.
   */
  function edgePath(e: WorkflowEdge): string {
    const from = e.from, to = e.to

    // Source and destination positions
    const fx = cx(from), fy = cy(from)
    const tx = cx(to),   ty = cy(to)

    // Back-edges (loop-back): arc below the cluster.
    if (e.kind === 'back') {
      const arcDepth = 30
      const y1 = bottom(from) + 3
      const y2 = bottom(to) + 3
      const arcY = Math.max(y1, y2) + arcDepth
      return `M ${fx} ${y1} Q ${(fx + tx) / 2} ${arcY} ${tx} ${y2}`
    }

    // Long edges that cross over the impl cluster: route below.
    // Specifically: dec_analysis_routing → flagged_human (far right)
    const longEdge = (from === 'dec_analysis_routing' && to === 'flagged_human')
                  || (from === 'dec_analysis_routing' && to === 'approved')
    if (longEdge && tx > fx + 400) {
      const arcY = SVG_H - 12
      return `M ${fx} ${bottom(from)} C ${fx} ${arcY} ${tx} ${arcY} ${tx} ${bottom(to)}`
    }

    // Normal forward edges: horizontal-biased cubic bezier.
    // Determine exit/entry points based on relative positions.
    let x1: number, y1: number, x2: number, y2: number
    const dx = tx - fx, dy = ty - fy

    if (Math.abs(dx) >= Math.abs(dy)) {
      // Primarily horizontal: exit right / enter left (or vice versa).
      if (dx >= 0) {
        x1 = right(from); y1 = fy
        x2 = left(to);   y2 = ty
      } else {
        x1 = left(from); y1 = fy
        x2 = right(to);  y2 = ty
      }
    } else {
      // Primarily vertical: exit bottom / enter top (or vice versa).
      if (dy >= 0) {
        x1 = fx; y1 = bottom(from)
        x2 = tx; y2 = top(to)
      } else {
        x1 = fx; y1 = top(from)
        x2 = tx; y2 = bottom(to)
      }
    }

    const bias = Math.abs(dx) * 0.4
    return `M ${x1} ${y1} C ${x1 + (dx >= 0 ? bias : -bias)} ${y1} ${x2 - (dx >= 0 ? bias : -bias)} ${y2} ${x2} ${y2}`
  }

  /** Whether a node is in POS (has a defined position). */
  function hasPos(id: string): boolean { return id in POS }

  function selectNode(n: WorkflowNode) {
    selected = selected?.id === n.id ? null : n
  }

  function kindLabel(kind: string): string {
    switch (kind) {
      case 'entry':     return 'Entry'
      case 'transient': return 'Transient'
      case 'active':    return 'Active'
      case 'decision':  return 'Decision'
      case 'terminal':  return 'Terminal'
      default: return kind
    }
  }
</script>

<!-- Intro prose -->
<div class="mx-auto max-w-4xl px-6 pt-8 pb-4">
  <h1 class="text-2xl font-semibold text-gray-900 dark:text-gray-100 mb-2">How it works</h1>
  <p class="text-sm text-gray-600 dark:text-gray-400 leading-relaxed max-w-2xl">
    This tool emulates a competent, considerate human reviewer.
    It triages each open dependabot PR, fixes and justifies when confident, or leaves a
    concise review when there is a high-confidence insight — and says nothing when there is not.
    <strong class="text-gray-800 dark:text-gray-200">Human attention is the budget.</strong>
    Click any node to learn what happens there.
  </p>
</div>

<!-- Graph -->
<div class="mx-auto max-w-6xl px-4 pb-6">
  {#if loading}
    <p class="text-sm text-gray-400 dark:text-gray-500 py-12 text-center animate-pulse">Loading workflow…</p>
  {:else if error}
    <p class="text-sm text-red-500 dark:text-red-400 py-12 text-center">{error}</p>
  {:else if graph}
    <div class="rounded-xl border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800/40 p-4 overflow-x-auto">
      <svg
        viewBox="0 0 {SVG_W} {SVG_H}"
        preserveAspectRatio="xMidYMid meet"
        class="w-full"
        style="min-width: 900px; min-height: 180px; max-height: 260px;"
        role="img"
        aria-label="PR workflow decision tree"
      >
        <defs>
          <marker id="wf-arrow" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
            <path d="M0,0 L0,7 L7,3.5 z" fill="#9ca3af" />
          </marker>
          <marker id="wf-arrow-back" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
            <path d="M0,0 L0,7 L7,3.5 z" fill="#f97316" opacity="0.7" />
          </marker>
          <marker id="wf-arrow-decision" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
            <path d="M0,0 L0,7 L7,3.5 z" fill="#6b7280" />
          </marker>
        </defs>

        <!-- Edges (drawn behind nodes) -->
        {#each graph.edges.filter(e => hasPos(e.from) && hasPos(e.to)) as e}
          {@const isBack = e.kind === 'back'}
          <path
            d={edgePath(e)}
            stroke={isBack ? '#f97316' : '#d1d5db'}
            stroke-width={isBack ? 1.5 : 1}
            stroke-opacity={isBack ? 0.7 : 0.85}
            stroke-dasharray={isBack ? '5 3' : undefined}
            fill="none"
            marker-end={isBack ? 'url(#wf-arrow-back)' : 'url(#wf-arrow)'}
          />
        {/each}

        <!-- Nodes -->
        {#each graph.nodes.filter(n => hasPos(n.id)) as n}
          {@const b = box(n.id)}
          {@const isSelected = selected?.id === n.id}
          <g
            class="cursor-pointer"
            onclick={() => selectNode(n)}
            role="button"
            tabindex="0"
            aria-label={n.label}
            onkeydown={ev => ev.key === 'Enter' && selectNode(n)}
          >
            {#if n.kind === 'decision'}
              <!-- Diamond -->
              <polygon
                points={diamondPoints(n.id)}
                fill={DECISION_FILL}
                stroke={isSelected ? '#3b82f6' : DECISION_STROKE}
                stroke-width={isSelected ? 2 : 1}
              />
            {:else}
              <!-- Rounded rect -->
              <rect
                x={b.x} y={b.y} width={b.w} height={b.h}
                rx={RX}
                fill={nodeFill(n)}
                stroke={isSelected ? '#3b82f6' : '#d1d5db'}
                stroke-width={isSelected ? 2 : 1}
              />
              {#if n.kind === 'terminal'}
                <!-- Dashed border for terminal nodes -->
                <rect
                  x={b.x + 2} y={b.y + 2} width={b.w - 4} height={b.h - 4}
                  rx={RX - 2}
                  fill="none"
                  stroke={isSelected ? '#3b82f6' : '#9ca3af'}
                  stroke-width="0.75"
                  stroke-dasharray="3 2"
                />
              {/if}
              {#if n.kind === 'entry'}
                <!-- Double border for entry node -->
                <rect
                  x={b.x - 3} y={b.y - 3} width={b.w + 6} height={b.h + 6}
                  rx={RX + 3}
                  fill="none"
                  stroke={isSelected ? '#3b82f6' : '#9ca3af'}
                  stroke-width="1"
                />
              {/if}
            {/if}
            <!-- Label text -->
            <text
              x={cx(n.id)} y={cy(n.id) + 1}
              text-anchor="middle"
              dominant-baseline="middle"
              font-size={n.kind === 'decision' ? '8' : '9'}
              font-family="ui-sans-serif,system-ui,sans-serif"
              font-weight="500"
              fill="#374151"
              pointer-events="none"
            >{n.label}</text>
          </g>
        {/each}
      </svg>

      <!-- Legend -->
      <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-gray-400 px-1">
        <span class="flex items-center gap-1.5">
          <span class="inline-block w-6 h-3.5 rounded" style="background:#bfdbfe;border:1px solid #d1d5db"></span>
          stage node
        </span>
        <span class="flex items-center gap-1.5">
          <span class="inline-block w-4 h-4 rotate-45" style="background:#f3f4f6;border:1px solid #6b7280"></span>
          decision
        </span>
        <span class="flex items-center gap-1.5">
          <svg width="20" height="6"><path d="M0,3 L18,3" stroke="#d1d5db" stroke-width="1.5" marker-end="url(#wf-arrow)"/></svg>
          transition
        </span>
        <span class="flex items-center gap-1.5">
          <svg width="20" height="6"><path d="M0,3 L18,3" stroke="#f97316" stroke-width="1.5" stroke-dasharray="4 2"/></svg>
          loop-back
        </span>
        <span class="flex items-center gap-1.5">
          <span class="inline-block w-5 h-3.5 rounded" style="background:#f3f4f6;border:1px dashed #9ca3af"></span>
          terminal
        </span>
        <span class="ml-auto text-gray-400 italic">click a node for details</span>
      </div>
    </div>

    <!-- Node detail panel -->
    {#if selected}
      <div class="mt-4 rounded-xl border border-blue-200 dark:border-blue-800 bg-blue-50 dark:bg-blue-900/20 px-5 py-4 max-w-2xl">
        <div class="flex items-start justify-between gap-2 mb-2">
          <div>
            <span class="text-xs font-medium text-blue-600 dark:text-blue-400 uppercase tracking-wide">
              {kindLabel(selected.kind)}
              {#if selected.phase}· {selected.phase}{/if}
            </span>
            <h2 class="text-base font-semibold text-gray-900 dark:text-gray-100 mt-0.5">{selected.label}</h2>
          </div>
          <button
            type="button"
            onclick={() => (selected = null)}
            class="text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 p-1 rounded shrink-0"
            aria-label="Close"
          >
            <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
              <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12"/>
            </svg>
          </button>
        </div>
        <p class="text-sm text-gray-700 dark:text-gray-300 leading-relaxed">{selected.summary}</p>
        {#if selected.detail}
          <p class="mt-2 text-sm text-gray-500 dark:text-gray-400 leading-relaxed">{selected.detail}</p>
        {/if}
        {#if selected.where}
          <p class="mt-2 text-xs font-mono text-gray-400 dark:text-gray-500">⌥ {selected.where}</p>
        {/if}
      </div>
    {/if}

    <!-- Stage list (text fallback / quick reference) -->
    <div class="mt-6">
      <h2 class="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-3">All stages</h2>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-2">
        {#each graph.nodes.filter(n => n.kind !== 'decision') as n}
          <button
            type="button"
            onclick={() => selectNode(n)}
            class="text-left rounded-lg border px-3 py-2 transition-colors text-sm
              {selected?.id === n.id
                ? 'border-blue-400 dark:border-blue-600 bg-blue-50 dark:bg-blue-900/30'
                : 'border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800/40 hover:border-gray-300 dark:hover:border-gray-600'}"
          >
            <div class="flex items-center gap-2 mb-0.5">
              <span
                class="inline-block w-3 h-3 rounded-sm shrink-0"
                style="background:{STAGE_FILL[n.id] ?? '#e5e7eb'}"
              ></span>
              <span class="font-medium text-gray-800 dark:text-gray-200">{n.label}</span>
              <span class="ml-auto text-xs text-gray-400 dark:text-gray-500 font-mono">{n.id}</span>
            </div>
            <p class="text-xs text-gray-500 dark:text-gray-400 leading-snug line-clamp-2 ml-5">{n.summary}</p>
          </button>
        {/each}
      </div>
    </div>
  {/if}
</div>
