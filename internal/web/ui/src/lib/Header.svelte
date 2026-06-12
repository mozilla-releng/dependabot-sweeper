<script lang="ts">
  import type { PRProgress, Status } from './api'

  let { prs, status, connected, currentView, onNavClick }: {
    prs: PRProgress[]
    status: Status | null
    connected: boolean
    currentView: string
    onNavClick: (path: string) => void
  } = $props()

  function fmtTime(s: string | undefined): string {
    if (!s || s.startsWith('0001-01-01')) return '—'
    const d = new Date(s)
    return isNaN(d.getTime()) ? '—' : d.toLocaleTimeString()
  }

  // Stage groupings for the summary chips.
  const activeStages = new Set([
    'analysing', 'reviewing',
    'impl_starting', 'impl_running', 'impl_resuming', 'waiting_ci',
  ])
  const settlingStages = new Set(['ci_settling'])
  const doneStages = new Set(['approved', 'finalized'])
  const flaggedStages = new Set(['flagged_human', 'gave_up', 'error'])

  const counts = $derived({
    active:   prs.filter(p => activeStages.has(p.stage)).length,
    settling: prs.filter(p => settlingStages.has(p.stage)).length,
    done:     prs.filter(p => doneStages.has(p.stage)).length,
    flagged:  prs.filter(p => flaggedStages.has(p.stage)).length,
  })
</script>

<header class="border-b border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-900 px-6 py-4">
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="flex items-center gap-3">
      <h1 class="text-lg font-semibold text-gray-900 dark:text-gray-100">
        dependabot review
      </h1>
      <!-- connection dot -->
      <span
        class="inline-block h-2 w-2 rounded-full {connected
          ? 'bg-green-500'
          : 'bg-red-400 animate-pulse'}"
        title={connected ? 'live' : 'reconnecting…'}
      ></span>
      <!-- nav links -->
      <nav class="flex items-center gap-1 ml-2">
        <button
          type="button"
          onclick={() => onNavClick('/')}
          class="text-xs px-2.5 py-1 rounded-md transition-colors
            {currentView === 'dashboard'
              ? 'bg-blue-100 dark:bg-blue-900/40 text-blue-700 dark:text-blue-300 font-medium'
              : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 hover:bg-gray-100 dark:hover:bg-gray-800'}"
        >Dashboard</button>
        <button
          type="button"
          onclick={() => onNavClick('/how-it-works')}
          class="text-xs px-2.5 py-1 rounded-md transition-colors
            {currentView === 'explainer'
              ? 'bg-blue-100 dark:bg-blue-900/40 text-blue-700 dark:text-blue-300 font-medium'
              : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200 hover:bg-gray-100 dark:hover:bg-gray-800'}"
        >How it works</button>
      </nav>
    </div>

    <!-- stage summary chips -->
    <div class="flex flex-wrap items-center gap-2 text-xs">
      <span class="rounded-full bg-gray-100 dark:bg-gray-800 px-2.5 py-0.5 font-medium text-gray-700 dark:text-gray-300">
        {prs.length} PR{prs.length === 1 ? '' : 's'}
      </span>
      {#if counts.active > 0}
        <span class="rounded-full bg-orange-100 dark:bg-orange-900/40 px-2.5 py-0.5 font-medium text-orange-800 dark:text-orange-300">
          {counts.active} active
        </span>
      {/if}
      {#if counts.settling > 0}
        <span class="rounded-full bg-amber-100 dark:bg-amber-900/40 px-2.5 py-0.5 font-medium text-amber-800 dark:text-amber-300">
          {counts.settling} settling
        </span>
      {/if}
      {#if counts.done > 0}
        <span class="rounded-full bg-green-100 dark:bg-green-900/40 px-2.5 py-0.5 font-medium text-green-800 dark:text-green-300">
          {counts.done} done
        </span>
      {/if}
      {#if counts.flagged > 0}
        <span class="rounded-full bg-red-100 dark:bg-red-900/40 px-2.5 py-0.5 font-medium text-red-800 dark:text-red-300">
          {counts.flagged} flagged
        </span>
      {/if}
    </div>

    <!-- scan status -->
    {#if status}
      <p class="text-xs text-gray-500 dark:text-gray-400 tabular-nums">
        in-flight: <span class="font-medium text-gray-700 dark:text-gray-300">{status.in_flight}</span>
        &nbsp;·&nbsp;
        last: <span class="font-medium text-gray-700 dark:text-gray-300">{fmtTime(status.last_scan)}</span>
        &nbsp;·&nbsp;
        next: <span class="font-medium text-gray-700 dark:text-gray-300">{fmtTime(status.next_scan)}</span>
      </p>
    {/if}
  </div>
</header>
