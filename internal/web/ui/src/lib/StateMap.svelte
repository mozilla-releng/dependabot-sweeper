<script lang="ts">
  import { onMount } from 'svelte'
  import type { PRProgress, WorkflowGraph, WorkflowEdge } from './api'
  import type { Phase } from './stages'
  import { STAGE_FILL, stageLabel } from './stages'
  import { fetchWorkflow } from './api'

  let {
    prs,
    activeFilter,
    onNodeClick,
  }: {
    prs: PRProgress[]
    activeFilter: Phase | null
    onNodeClick: (phase: Phase) => void
  } = $props()

  // Count PRs per stage.
  const stageCounts = $derived(() => {
    const m: Record<string, number> = {}
    for (const pr of prs) {
      m[pr.stage] = (m[pr.stage] ?? 0) + 1
    }
    return m
  })

  // ── Spec-derived edges ─────────────────────────────────────────────────────
  // Edges are derived from the workflow spec (GET /api/v1/workflow) by
  // condensing decision nodes: for each stage node A, find all stage nodes B
  // reachable from A via paths that only pass through decision nodes.
  // Back-kind is determined visually: if B is to the LEFT of A in the layout,
  // the derived edge is a loop-back (dashed orange).

  let specGraph = $state<WorkflowGraph | null>(null)

  onMount(async () => {
    try { specGraph = await fetchWorkflow() } catch { /* use empty edges */ }
  })

  type EdgeDef = { from: string; to: string; back?: boolean }

  // Condense decision nodes out of the spec to produce stage→stage edges.
  function condenseEdges(graph: WorkflowGraph): EdgeDef[] {
    const kindOf = new Map(graph.nodes.map(n => [n.id, n.kind]))
    const adj = new Map<string, WorkflowEdge[]>()
    for (const e of graph.edges) {
      const list = adj.get(e.from) ?? []
      list.push(e)
      adj.set(e.from, list)
    }

    const result: EdgeDef[] = []
    const seen = new Set<string>()

    for (const start of graph.nodes) {
      if (kindOf.get(start.id) === 'decision') continue
      if (!(start.id in nmap)) continue  // only process nodes visible in this map

      // BFS through decision nodes; firstBack = was the first hop a back edge?
      const queue: Array<[string, boolean]> = []
      for (const e of adj.get(start.id) ?? []) {
        queue.push([e.to, e.kind === 'back'])
      }
      const visitedDec = new Set<string>()

      while (queue.length > 0) {
        const [nid, firstBack] = queue.shift()!
        const nkind = kindOf.get(nid)

        if (nkind !== 'decision') {
          // Stage node found — add edge if both ends are in the layout
          if (nid in nmap && nid !== start.id) {
            const key = `${start.id}→${nid}`
            if (!seen.has(key)) {
              seen.add(key)
              // Visual back: destination is to the LEFT of source in the map.
              const visualBack = nmap[nid].x < nmap[start.id].x
              result.push({ from: start.id, to: nid, back: visualBack || firstBack })
            }
          }
        } else if (!visitedDec.has(nid)) {
          // Decision node — traverse through it, preserving firstBack
          visitedDec.add(nid)
          for (const e of adj.get(nid) ?? []) {
            queue.push([e.to, firstBack])
          }
        }
      }
    }

    return result
  }

  // Derived edge list: from spec when loaded, empty otherwise.
  const edges = $derived(specGraph ? condenseEdges(specGraph) : [])

  // ── Node layout ────────────────────────────────────────────────────────
  // We use a fixed SVG coordinate space (viewBox). Each node is a rounded rect.
  // Nodes are arranged in a logical left-to-right flow with the impl⇄CI⇄review
  // loop in the middle tier.

  type NodeDef = {
    id: string
    label: string
    x: number; y: number; w: number; h: number
    fill: string
    phase: Phase
  }

  // SVG canvas: 900 × 200
  const W = 900, H = 200
  const NW = 90, NH = 36 // node width, height
  const RX = 8           // corner radius

  const nodes: NodeDef[] = [
    // Tier 0 — entry
    { id: 'pending',       label: 'Pending',     x:  20, y:  82, w: NW, h: NH, fill: STAGE_FILL.pending,       phase: 'Queued' },
    { id: 'ci_settling',   label: 'CI settling', x:  20, y: 136, w: NW, h: NH, fill: STAGE_FILL.ci_settling,   phase: 'Queued' },

    // Tier 1 — analysis
    { id: 'analysing',     label: 'Analysing',   x: 148, y:  82, w: NW, h: NH, fill: STAGE_FILL.analysing,     phase: 'Analysing' },

    // Tier 2 — impl cluster (three rows)
    { id: 'impl_starting', label: 'Impl start',  x: 280, y:  28, w: NW, h: NH, fill: STAGE_FILL.impl_starting, phase: 'Implementing' },
    { id: 'impl_running',  label: 'Impl running',x: 280, y:  82, w: NW, h: NH, fill: STAGE_FILL.impl_running,  phase: 'Implementing' },
    { id: 'impl_resuming', label: 'Impl resume', x: 280, y: 136, w: NW, h: NH, fill: STAGE_FILL.impl_resuming, phase: 'Implementing' },

    // Tier 3 — CI wait
    { id: 'waiting_ci',    label: 'Waiting CI',  x: 420, y:  55, w: NW, h: NH, fill: STAGE_FILL.waiting_ci,   phase: 'CI+Review' },

    // Tier 4 — review
    { id: 'reviewing',     label: 'Reviewing',   x: 555, y:  55, w: NW, h: NH, fill: STAGE_FILL.reviewing,    phase: 'CI+Review' },

    // Tier 5 — terminal
    { id: 'approved',      label: 'Approved',    x: 700, y:  28, w: NW, h: NH, fill: STAGE_FILL.approved,     phase: 'Done+Flagged' },
    { id: 'finalized',     label: 'Finalized',   x: 700, y:  82, w: NW, h: NH, fill: STAGE_FILL.finalized,    phase: 'Done+Flagged' },
    { id: 'skipped',       label: 'Skipped',     x: 820, y:  28, w: NW, h: NH, fill: STAGE_FILL.skipped,      phase: 'Done+Flagged' },
    { id: 'flagged_human', label: 'Flagged',     x: 700, y: 136, w: NW, h: NH, fill: STAGE_FILL.flagged_human, phase: 'Done+Flagged' },
    { id: 'gave_up',       label: 'Gave up',     x: 820, y: 136, w: NW, h: NH, fill: STAGE_FILL.gave_up,      phase: 'Done+Flagged' },
    { id: 'error',         label: 'Error',       x: 820, y:  82, w: NW, h: NH, fill: STAGE_FILL.error,        phase: 'Done+Flagged' },
  ]

  // Node centre helper.
  function cx(n: NodeDef) { return n.x + n.w / 2 }
  function cy(n: NodeDef) { return n.y + n.h / 2 }
  function right(n: NodeDef) { return n.x + n.w }
  function bottom(n: NodeDef) { return n.y + n.h }

  // Build a map for quick lookup.
  const nmap = Object.fromEntries(nodes.map(n => [n.id, n]))

  // ── Edge rendering ────────────────────────────────────────────────────
  // Edges are derived from the workflow spec (fetched above); see condenseEdges().
  // Each edge is a cubic Bézier from src centre-right to dst centre-left.
  // Back-edges (loop-backs) arc below the cluster in orange dashed.

  function edgePath(e: EdgeDef): string {
    const src = nmap[e.from]
    const dst = nmap[e.to]
    if (!src || !dst) return ''
    const x1 = right(src), y1 = cy(src)
    const x2 = dst.x, y2 = cy(dst)
    if (e.back) {
      // Back-edges: quadratic Bézier arcing below the cluster.
      const midX = (x1 + x2) / 2
      const arcY = Math.max(y1, y2) + 55  // arc depth
      return `M ${x1} ${y1} Q ${midX} ${arcY} ${x2} ${y2}`
    }
    // Simple horizontal-ish line with a short cubic to avoid sharp kinks.
    const dx = (x2 - x1) * 0.4
    return `M ${x1} ${y1} C ${x1 + dx} ${y1} ${x2 - dx} ${y2} ${x2} ${y2}`
  }

  // Whether a node has active PRs (drives a pulsing ring).
  function hasActive(id: string) {
    return (stageCounts()[id] ?? 0) > 0
  }

  // Whether a node's phase is the active filter.
  function isFiltered(n: NodeDef) {
    return activeFilter !== null && activeFilter !== n.phase
  }
</script>

<!--
  Pure SVG state map. Clicking a node fires onNodeClick with its phase so the
  board can filter. No D3 — just SVG + Svelte reactivity.
-->
<div class="w-full overflow-x-auto rounded-lg border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800/40 py-2">
  <svg
    viewBox="0 0 {W} {H}"
    preserveAspectRatio="xMidYMid meet"
    class="w-full"
    style="min-width: 600px; max-height: 180px;"
    role="img"
    aria-label="PR stage state machine"
  >
    <!-- Arrowhead marker -->
    <defs>
      <marker id="arrow" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
        <path d="M0,0 L0,7 L7,3.5 z" fill="#9ca3af" />
      </marker>
      <marker id="arrow-back" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto">
        <path d="M0,0 L0,7 L7,3.5 z" fill="#f97316" opacity="0.7" />
      </marker>
    </defs>

    <!-- Edges (drawn behind nodes) -->
    {#each edges as e}
      <path
        d={edgePath(e)}
        stroke={e.back ? '#f97316' : '#d1d5db'}
        stroke-width={e.back ? 1.5 : 1}
        stroke-opacity={e.back ? 0.6 : 0.8}
        stroke-dasharray={e.back ? '4 3' : undefined}
        fill="none"
        marker-end={e.back ? 'url(#arrow-back)' : 'url(#arrow)'}
      />
    {/each}

    <!-- Nodes -->
    {#each nodes as n}
      <g
        class="cursor-pointer"
        onclick={() => onNodeClick(n.phase)}
        role="button"
        tabindex="0"
        aria-label="{stageLabel(n.id)} ({stageCounts()[n.id] ?? 0})"
        onkeydown={(ev) => ev.key === 'Enter' && onNodeClick(n.phase)}
        opacity={isFiltered(n) ? 0.35 : 1}
      >
        <!-- Active-PR pulsing ring -->
        {#if hasActive(n.id)}
          <rect
            x={n.x - 3} y={n.y - 3} width={n.w + 6} height={n.h + 6}
            rx={RX + 3}
            fill="none"
            stroke="#3b82f6"
            stroke-width="2"
            opacity="0.5"
          >
            <animate attributeName="opacity" values="0.5;0.1;0.5" dur="2s" repeatCount="indefinite" />
          </rect>
        {/if}

        <!-- Node body -->
        <rect
          x={n.x} y={n.y} width={n.w} height={n.h}
          rx={RX}
          fill={n.fill}
          stroke={activeFilter === n.phase ? '#3b82f6' : '#d1d5db'}
          stroke-width={activeFilter === n.phase ? 2 : 1}
        />

        <!-- Label -->
        <text
          x={cx(n)} y={n.y + NH / 2 - 4}
          text-anchor="middle"
          font-size="9"
          font-family="ui-sans-serif,system-ui,sans-serif"
          font-weight="500"
          fill="#374151"
        >{n.label}</text>

        <!-- Count badge (only when > 0) -->
        {#if (stageCounts()[n.id] ?? 0) > 0}
          <circle cx={n.x + n.w - 8} cy={n.y + 8} r="9" fill="#3b82f6" />
          <text
            x={n.x + n.w - 8} y={n.y + 12}
            text-anchor="middle"
            font-size="8"
            font-family="ui-monospace,monospace"
            font-weight="700"
            fill="#fff"
          >{stageCounts()[n.id]}</text>
        {/if}
      </g>
    {/each}
  </svg>
</div>
