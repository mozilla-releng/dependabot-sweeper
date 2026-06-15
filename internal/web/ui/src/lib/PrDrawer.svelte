<script lang="ts">
  import type { PRProgress } from './api'
  import StageBadge from './StageBadge.svelte'
  import StageTimeline from './StageTimeline.svelte'
  import CiChecks from './CiChecks.svelte'
  import AnalyserVerdict from './AnalyserVerdict.svelte'
  import RunMeta from './RunMeta.svelte'
  import AgentLogTail from './AgentLogTail.svelte'

  let {
    pr,
    onClose,
  }: {
    pr: PRProgress
    onClose: () => void
  } = $props()

  type Tab = 'timeline' | 'ci' | 'analysis' | 'meta' | 'log'
  let activeTab = $state<Tab>('timeline')

  const tabs: { id: Tab; label: string }[] = [
    { id: 'timeline', label: 'Timeline' },
    { id: 'ci',       label: 'CI' },
    { id: 'analysis', label: 'Analysis' },
    { id: 'meta',     label: 'Meta' },
    { id: 'log',      label: 'Agent log' },
  ]

  const verDiff = $derived(
    pr.old_version && pr.new_version
      ? `${pr.old_version} → ${pr.new_version}`
      : pr.new_version ?? ''
  )

  // Close on Escape key.
  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') onClose()
  }
</script>

<svelte:window onkeydown={onKeydown} />

<!-- Slide-in drawer panel -->
<div
  class="flex flex-col border-l border-gray-200 dark:border-gray-700
         bg-white dark:bg-gray-900 overflow-hidden
         w-80 xl:w-96 shrink-0"
>
  <!-- Drawer header -->
  <div class="flex items-start justify-between gap-2 px-4 py-3 border-b border-gray-100 dark:border-gray-800">
    <div class="min-w-0">
      <div class="flex items-center gap-2 flex-wrap">
        {#if pr.url}
          <a href={pr.url} target="_blank" rel="noopener noreferrer"
             class="font-mono font-bold text-gray-900 dark:text-gray-100 hover:underline">#{pr.pr_number}</a>
        {:else}
          <span class="font-mono font-bold text-gray-900 dark:text-gray-100">#{pr.pr_number}</span>
        {/if}
        <StageBadge stage={pr.stage} />
      </div>
      <p class="mt-0.5 text-sm font-medium text-gray-700 dark:text-gray-300 truncate" title={pr.package_name}>
        {pr.package_name}
      </p>
      {#if verDiff}
        <p class="text-xs font-mono text-gray-500 dark:text-gray-400 mt-0.5">{verDiff}</p>
      {/if}
    </div>
    <button
      type="button"
      onclick={onClose}
      class="shrink-0 text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 p-1 rounded"
      aria-label="Close"
    >
      <svg xmlns="http://www.w3.org/2000/svg" class="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="2">
        <path stroke-linecap="round" stroke-linejoin="round" d="M6 18L18 6M6 6l12 12"/>
      </svg>
    </button>
  </div>

  <!-- Tabs -->
  <div class="flex border-b border-gray-100 dark:border-gray-800 overflow-x-auto">
    {#each tabs as tab}
      <button
        type="button"
        onclick={() => (activeTab = tab.id)}
        class="px-3 py-2 text-xs font-medium whitespace-nowrap transition-colors
          {activeTab === tab.id
            ? 'border-b-2 border-blue-500 text-blue-600 dark:text-blue-400'
            : 'text-gray-500 dark:text-gray-400 hover:text-gray-700 dark:hover:text-gray-200'}"
      >
        {tab.label}
      </button>
    {/each}
  </div>

  <!-- Tab content -->
  <div class="flex-1 overflow-y-auto px-4 py-3">
    {#if activeTab === 'timeline'}
      <StageTimeline history={pr.history ?? []} />
    {:else if activeTab === 'ci'}
      <CiChecks ci={pr.ci} />
    {:else if activeTab === 'analysis'}
      <AnalyserVerdict analysis={pr.analysis} />
    {:else if activeTab === 'meta'}
      <RunMeta {pr} />
    {:else if activeTab === 'log'}
      <AgentLogTail prNumber={pr.pr_number} sessionId={pr.session_id} />
    {/if}
  </div>
</div>
