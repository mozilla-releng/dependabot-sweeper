<script lang="ts">
  import type { PRProgress } from './api'
  import type { Phase } from './stages'
  import { PHASES, PHASE_STAGES } from './stages'
  import Column from './Column.svelte'

  let {
    prs,
    selectedPR,
    activeFilter,
    onCardClick,
    onPhaseClick,
  }: {
    prs: PRProgress[]
    selectedPR: number | null
    activeFilter: Phase | null
    onCardClick: (pr: PRProgress) => void
    onPhaseClick: (phase: Phase) => void
  } = $props()

  // Bucket PRs by phase. A PR goes into exactly one phase column.
  const columns = $derived(() => {
    const buckets = Object.fromEntries(PHASES.map(p => [p, [] as PRProgress[]])) as Record<Phase, PRProgress[]>
    for (const pr of prs) {
      for (const phase of PHASES) {
        if ((PHASE_STAGES[phase] as string[]).includes(pr.stage)) {
          buckets[phase].push(pr)
          break
        }
      }
    }
    return buckets
  })
</script>

<div class="flex gap-3 overflow-x-auto pb-2 min-h-24">
  {#each PHASES as phase}
    <Column
      {phase}
      prs={columns()[phase]}
      {selectedPR}
      {activeFilter}
      {onCardClick}
      {onPhaseClick}
    />
  {/each}
</div>
