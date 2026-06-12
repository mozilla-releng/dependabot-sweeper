<script lang="ts">
  import type { PRProgress } from './api'
  import type { Phase } from './stages'
  import PrCard from './PrCard.svelte'

  let {
    phase,
    prs,
    selectedPR,
    activeFilter,
    onCardClick,
    onPhaseClick,
  }: {
    phase: Phase
    prs: PRProgress[]
    selectedPR: number | null
    activeFilter: Phase | null
    onCardClick: (pr: PRProgress) => void
    onPhaseClick: (phase: Phase) => void
  } = $props()

  const isActive = $derived(activeFilter === phase)

  // Phase header accent colours.
  const headerColour: Record<Phase, string> = {
    'Queued':        'border-gray-300 dark:border-gray-600 text-gray-600 dark:text-gray-400',
    'Analysing':     'border-blue-300 dark:border-blue-700 text-blue-700 dark:text-blue-400',
    'Implementing':  'border-orange-300 dark:border-orange-700 text-orange-700 dark:text-orange-400',
    'CI+Review':     'border-amber-300 dark:border-amber-700 text-amber-700 dark:text-amber-400',
    'Done+Flagged':  'border-green-300 dark:border-green-700 text-green-700 dark:text-green-400',
  }
</script>

<div class="flex flex-col min-w-44 max-w-60 flex-1">
  <!-- Column header — click to filter -->
  <button
    type="button"
    class="mb-2 flex items-center justify-between rounded-md border px-3 py-1.5 text-xs font-semibold
      transition-colors {headerColour[phase]}
      {isActive
        ? 'bg-blue-50 dark:bg-blue-900/20 border-blue-400 dark:border-blue-500 !text-blue-700 dark:!text-blue-300'
        : 'bg-white dark:bg-gray-800/50 hover:bg-gray-50 dark:hover:bg-gray-700/50'}"
    onclick={() => onPhaseClick(phase)}
    title="{isActive ? 'Clear filter' : `Filter to ${phase}`}"
  >
    <span>{phase}</span>
    <span class="ml-2 rounded-full bg-gray-200 dark:bg-gray-700 px-1.5 py-0.5 font-mono tabular-nums text-gray-700 dark:text-gray-300">
      {prs.length}
    </span>
  </button>

  <!-- Cards -->
  <div class="flex flex-col gap-1.5">
    {#each prs as pr (pr.pr_number)}
      <PrCard
        {pr}
        selected={selectedPR === pr.pr_number}
        onclick={() => onCardClick(pr)}
      />
    {/each}

    {#if prs.length === 0}
      <p class="text-xs text-center text-gray-300 dark:text-gray-600 py-4 italic">empty</p>
    {/if}
  </div>
</div>
