<script lang="ts">
  import { fetchLog } from './api'

  let { prNumber, sessionId }: { prNumber: number; sessionId?: string } = $props()

  let logText = $state('')
  let loading = $state(false)
  let loaded = $state(false)
  let error = $state('')

  async function load() {
    loading = true
    error = ''
    try {
      logText = (await fetchLog(prNumber)) || '(no log yet)'
      loaded = true
    } catch (e) {
      error = String(e)
    } finally {
      loading = false
    }
  }
</script>

{#if !sessionId}
  <p class="text-xs text-gray-400 dark:text-gray-500 italic">No agent session active.</p>
{:else if !loaded && !loading}
  <button
    type="button"
    onclick={load}
    class="text-xs text-blue-500 dark:text-blue-400 hover:underline"
  >
    Load agent log
  </button>
{:else if loading}
  <p class="text-xs text-gray-400 dark:text-gray-500 animate-pulse">Loading…</p>
{:else if error}
  <p class="text-xs text-red-500 dark:text-red-400">{error}</p>
{:else}
  <div class="flex items-center justify-between mb-1">
    <span class="text-xs text-gray-500 dark:text-gray-400">Last 200 lines</span>
    <button type="button" onclick={load} class="text-xs text-blue-500 dark:text-blue-400 hover:underline">
      Refresh
    </button>
  </div>
  <pre class="max-h-64 overflow-auto rounded bg-gray-50 dark:bg-gray-900 p-2 text-xs text-gray-700 dark:text-gray-300 whitespace-pre-wrap break-all">{logText}</pre>
{/if}
