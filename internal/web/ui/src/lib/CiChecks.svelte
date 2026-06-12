<script lang="ts">
  import type { CIStatus, CheckDetail } from './api'

  let { ci }: { ci: CIStatus | undefined } = $props()

  // Segment bar widths.
  const total = $derived(ci?.total ?? 0)
  const passedPct = $derived(total > 0 ? (ci!.passed / total) * 100 : 0)
  const failedPct = $derived(total > 0 ? ((ci?.failed ?? 0) / total) * 100 : 0)
  const pendingPct = $derived(total > 0 ? ((ci?.pending ?? 0) / total) * 100 : 0)

  function conclusionIcon(check: CheckDetail): string {
    const c = check.conclusion ?? check.status
    if (c === 'success')   return '✓'
    if (c === 'failure' || c === 'timed_out') return '✗'
    return '○'
  }

  function conclusionClass(check: CheckDetail): string {
    const c = check.conclusion ?? check.status
    if (c === 'success') return 'text-green-600 dark:text-green-400'
    if (c === 'failure' || c === 'timed_out') return 'text-red-600 dark:text-red-400'
    return 'text-amber-600 dark:text-amber-400'
  }
</script>

{#if !ci}
  <p class="text-xs text-gray-400 dark:text-gray-500 italic">No CI data yet.</p>
{:else}
  <!-- Aggregate bar -->
  <div class="mb-3">
    <div class="flex items-center justify-between text-xs text-gray-600 dark:text-gray-400 mb-1">
      <span class="font-medium">{ci.state || 'unknown'}</span>
      <span class="tabular-nums">
        {ci.passed} pass · {ci.failed} fail · {ci.pending} pending of {ci.total}
      </span>
    </div>
    <div class="flex h-2 w-full rounded-full overflow-hidden bg-gray-100 dark:bg-gray-700">
      <div class="bg-green-400 dark:bg-green-600 transition-all duration-500" style="width: {passedPct}%"></div>
      <div class="bg-red-400 dark:bg-red-600 transition-all duration-500"   style="width: {failedPct}%"></div>
      <div class="bg-amber-300 dark:bg-amber-500 transition-all duration-500" style="width: {pendingPct}%"></div>
    </div>
  </div>

  <!-- Per-check list (failures first, then the rest) -->
  {#if ci.checks?.length}
    <ul class="space-y-1">
      {#each ci.checks as check}
        <li class="flex items-start gap-2 text-xs">
          <span class="shrink-0 font-mono font-bold {conclusionClass(check)}">{conclusionIcon(check)}</span>
          <div class="flex-1 min-w-0">
            <div class="flex items-center gap-1.5 flex-wrap">
              <span class="font-medium text-gray-800 dark:text-gray-200 truncate">{check.name}</span>
              <span class="text-gray-400 dark:text-gray-500">{check.status}</span>
            </div>
            {#if check.details_url}
              <a
                href={check.details_url}
                target="_blank"
                rel="noopener noreferrer"
                class="text-blue-500 dark:text-blue-400 hover:underline text-xs"
              >view logs ↗</a>
            {/if}
          </div>
        </li>
      {/each}
    </ul>
  {:else if ci.failures?.length}
    <!-- Fallback: show failures if checks is empty -->
    <ul class="space-y-1">
      {#each ci.failures as check}
        <li class="flex items-start gap-2 text-xs">
          <span class="shrink-0 font-mono font-bold text-red-600 dark:text-red-400">✗</span>
          <div class="flex-1 min-w-0">
            <span class="font-medium text-gray-800 dark:text-gray-200 truncate">{check.name}</span>
            {#if check.details_url}
              <a
                href={check.details_url}
                target="_blank"
                rel="noopener noreferrer"
                class="ml-2 text-blue-500 dark:text-blue-400 hover:underline"
              >logs ↗</a>
            {/if}
          </div>
        </li>
      {/each}
    </ul>
  {:else}
    <p class="text-xs text-gray-400 dark:text-gray-500 italic">No per-check details available.</p>
  {/if}
{/if}
