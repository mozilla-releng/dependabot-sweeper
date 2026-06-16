<script lang="ts">
  import type { PRProgress } from './api'
  import StageBadge from './StageBadge.svelte'
  import { stageProgress } from './stages'

  let {
    pr,
    selected = false,
    onclick,
  }: {
    pr: PRProgress
    selected?: boolean
    onclick?: () => void
  } = $props()

  // Progress bar width (0..100%).
  const progress = $derived(Math.round(stageProgress(pr.stage) * 100))

  // CI state colour for the indicator dot.
  const ciDot = $derived(() => {
    const s = pr.ci?.state ?? ''
    if (s === 'success') return 'bg-green-500'
    if (s === 'failure') return 'bg-red-500'
    if (s === 'pending') return 'bg-amber-400 animate-pulse'
    return 'bg-gray-300 dark:bg-gray-600'
  })

  // Version diff label.
  const verDiff = $derived(
    pr.old_version && pr.new_version
      ? `${pr.old_version} → ${pr.new_version}`
      : pr.new_version ?? ''
  )
</script>

<button
  type="button"
  class="w-full text-left rounded-lg border p-3 shadow-xs transition-colors
    {selected
      ? 'border-blue-400 dark:border-blue-500 bg-blue-50 dark:bg-blue-900/20'
      : 'border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800/50 hover:border-gray-300 dark:hover:border-gray-600'}"
  onclick={onclick}
>
  <!-- Row 1: PR number + badge + CI dot -->
  <div class="flex items-center justify-between gap-2 min-w-0">
    <div class="flex items-center gap-1.5 min-w-0">
      {#if pr.url}
        <a href={pr.url} target="_blank" rel="noopener noreferrer"
           class="font-mono text-sm font-semibold text-gray-900 dark:text-gray-100 shrink-0 hover:underline"
           onclick={(e) => e.stopPropagation()}>#{pr.pr_number}</a>
      {:else}
        <span class="font-mono text-sm font-semibold text-gray-900 dark:text-gray-100 shrink-0">
          #{pr.pr_number}
        </span>
      {/if}
      {#if pr.replacement_pr}
        <span class="text-gray-400 dark:text-gray-500 text-xs shrink-0">→</span>
        {#if pr.replacement_pr_url}
          <a href={pr.replacement_pr_url} target="_blank" rel="noopener noreferrer"
             class="font-mono text-sm font-semibold text-gray-400 dark:text-gray-500 shrink-0 hover:underline"
             onclick={(e) => e.stopPropagation()}>#{pr.replacement_pr}</a>
        {:else}
          <span class="font-mono text-sm font-semibold text-gray-400 dark:text-gray-500 shrink-0">#{pr.replacement_pr}</span>
        {/if}
      {/if}
      <StageBadge stage={pr.stage} />
    </div>
    <!-- CI state dot -->
    <span
      class="inline-block h-2 w-2 rounded-full shrink-0 {ciDot()}"
      title={pr.ci ? `CI: ${pr.ci.state} (${pr.ci.passed}/${pr.ci.total} passed)` : 'CI: unknown'}
    ></span>
  </div>

  <!-- Row 2: package + ecosystem chip + bump chip -->
  <div class="mt-1.5 flex flex-wrap items-center gap-1.5 text-xs">
    <span class="truncate max-w-40 text-gray-700 dark:text-gray-300 font-medium" title={pr.package_name}>
      {pr.package_name || '—'}
    </span>
    {#if pr.ecosystem}
      <span class="rounded bg-gray-100 dark:bg-gray-700 px-1.5 py-0.5 text-gray-500 dark:text-gray-400">
        {pr.ecosystem}
      </span>
    {/if}
    {#if pr.bump_type}
      <span class="rounded bg-gray-100 dark:bg-gray-700 px-1.5 py-0.5 text-gray-500 dark:text-gray-400">
        {pr.bump_type}
      </span>
    {/if}
  </div>

  <!-- Row 3: version diff -->
  {#if verDiff}
    <p class="mt-1 text-xs font-mono text-gray-500 dark:text-gray-400 truncate">{verDiff}</p>
  {/if}

  <!-- Row 4: progress bar -->
  <div class="mt-2 h-1 w-full rounded-full bg-gray-100 dark:bg-gray-700 overflow-hidden">
    <div
      class="h-1 rounded-full transition-all duration-500
        {pr.stage === 'error' || pr.stage === 'gave_up' || pr.stage === 'flagged_human'
          ? 'bg-red-400 dark:bg-red-600'
          : pr.stage === 'approved' || pr.stage === 'finalized'
            ? 'bg-green-400 dark:bg-green-600'
            : 'bg-blue-400 dark:bg-blue-600'}"
      style="width: {progress}%"
    ></div>
  </div>
</button>
