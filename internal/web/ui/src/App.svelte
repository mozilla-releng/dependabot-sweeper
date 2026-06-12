<script lang="ts">
  import { onMount } from 'svelte'
  import { fetchPRs, fetchStatus } from './lib/api'
  import type { PRProgress, Status } from './lib/api'
  import type { Phase } from './lib/stages'
  import { PHASE_STAGES } from './lib/stages'
  import Header from './lib/Header.svelte'
  import StateMap from './lib/StateMap.svelte'
  import PipelineBoard from './lib/PipelineBoard.svelte'
  import PrDrawer from './lib/PrDrawer.svelte'
  import WorkflowExplainer from './lib/WorkflowExplainer.svelte'

  // ── Minimal client-side router ─────────────────────────────────────────────
  // Two views: '/' → dashboard, '/how-it-works' → explainer.
  // handleSPA already serves index.html for any unknown path, so deep-links work.

  type View = 'dashboard' | 'explainer'

  function pathToView(path: string): View {
    return path === '/how-it-works' ? 'explainer' : 'dashboard'
  }

  let currentView = $state<View>(pathToView(window.location.pathname))

  function navigate(path: string) {
    history.pushState({}, '', path)
    currentView = pathToView(path)
  }

  // ── Dashboard state ────────────────────────────────────────────────────────

  let prs = $state<PRProgress[]>([])
  let status = $state<Status | null>(null)
  let connected = $state(false)
  let error = $state('')

  // Filter: null = show all; set by clicking a map node or column header.
  let activeFilter = $state<Phase | null>(null)
  // Selected PR (drawer open).
  let selectedPR = $state<PRProgress | null>(null)

  // PRs visible in the board after applying the active phase filter.
  const visiblePRs = $derived(
    activeFilter
      ? prs.filter(pr => (PHASE_STAGES[activeFilter!] as string[]).includes(pr.stage))
      : prs
  )

  function toggleFilter(phase: Phase) {
    activeFilter = activeFilter === phase ? null : phase
  }

  function openDrawer(pr: PRProgress) {
    // Re-clicking the same card toggles the drawer.
    selectedPR = selectedPR?.pr_number === pr.pr_number ? null : pr
  }

  function closeDrawer() {
    selectedPR = null
  }

  async function refresh() {
    try {
      ;[prs, status] = await Promise.all([fetchPRs(), fetchStatus()])
      // Keep the drawer in sync: if the drawer is open, update its PR data.
      if (selectedPR !== null) {
        const updated = prs.find(p => p.pr_number === selectedPR!.pr_number)
        selectedPR = updated ?? null
      }
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  onMount(() => {
    refresh()

    const es = new EventSource('/api/v1/events')
    es.addEventListener('update', () => refresh())
    es.onopen = () => { connected = true }
    es.onerror = () => { connected = false }

    // Handle browser back/forward navigation.
    const onPop = () => { currentView = pathToView(window.location.pathname) }
    window.addEventListener('popstate', onPop)

    return () => {
      es.close()
      window.removeEventListener('popstate', onPop)
    }
  })
</script>

<div class="min-h-screen bg-gray-50 dark:bg-gray-900 text-gray-900 dark:text-gray-100 flex flex-col">
  <Header
    {prs} {status} {connected}
    {currentView}
    onNavClick={navigate}
  />

  {#if currentView === 'explainer'}
    <!-- How it works page -->
    <main class="flex-1 overflow-y-auto">
      <WorkflowExplainer />
    </main>
  {:else}
    <!-- Dashboard -->
    <div class="flex flex-1 overflow-hidden">
      <!-- Main content area -->
      <main class="flex-1 overflow-y-auto overflow-x-hidden min-w-0">
        <div class="mx-auto max-w-7xl px-4 py-4 space-y-4">
          <!-- Error banner -->
          {#if error}
            <div class="rounded-lg border border-red-200 dark:border-red-800 bg-red-50 dark:bg-red-900/20 px-4 py-3 text-sm text-red-700 dark:text-red-400">
              {error}
            </div>
          {/if}

          <!-- State machine map -->
          <StateMap
            {prs}
            {activeFilter}
            onNodeClick={toggleFilter}
          />

          <!-- Active filter chip -->
          {#if activeFilter}
            <div class="flex items-center gap-2 text-sm">
              <span class="text-gray-500 dark:text-gray-400">Filtering:</span>
              <span class="rounded-full bg-blue-100 dark:bg-blue-900/40 px-3 py-0.5 font-medium text-blue-700 dark:text-blue-300">
                {activeFilter}
              </span>
              <button
                type="button"
                onclick={() => (activeFilter = null)}
                class="text-gray-400 hover:text-gray-600 dark:hover:text-gray-200 text-xs hover:underline"
              >
                clear
              </button>
            </div>
          {/if}

          <!-- Pipeline board -->
          {#if prs.length === 0 && !error}
            <p class="text-center text-sm text-gray-400 dark:text-gray-500 py-12">
              No open dependabot PRs — waiting for the next scan…
            </p>
          {:else}
            <PipelineBoard
              prs={visiblePRs}
              selectedPR={selectedPR?.pr_number ?? null}
              {activeFilter}
              onCardClick={openDrawer}
              onPhaseClick={toggleFilter}
            />
          {/if}
        </div>
      </main>

      <!-- Slide-in drawer (rendered when a PR is selected) -->
      {#if selectedPR}
        <PrDrawer pr={selectedPR} onClose={closeDrawer} />
      {/if}
    </div>
  {/if}
</div>
