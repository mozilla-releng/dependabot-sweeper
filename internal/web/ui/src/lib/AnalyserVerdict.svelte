<script lang="ts">
  import type { AgentAnalysis } from './api'

  let { analysis }: { analysis: AgentAnalysis | undefined } = $props()

  const recColour = $derived(() => {
    if (!analysis) return ''
    const r = analysis.recommendation
    if (r === 'approve') return 'text-green-700 dark:text-green-400 bg-green-50 dark:bg-green-900/20 border-green-200 dark:border-green-800'
    if (r === 'needs_changes') return 'text-orange-700 dark:text-orange-400 bg-orange-50 dark:bg-orange-900/20 border-orange-200 dark:border-orange-800'
    return 'text-red-700 dark:text-red-400 bg-red-50 dark:bg-red-900/20 border-red-200 dark:border-red-800'
  })

  const confColour = $derived(() => {
    if (!analysis) return 'text-gray-500'
    if (analysis.confidence === 'high') return 'text-green-600 dark:text-green-400'
    if (analysis.confidence === 'low')  return 'text-red-600 dark:text-red-400'
    return 'text-amber-600 dark:text-amber-400'
  })
</script>

{#if !analysis}
  <p class="text-xs text-gray-400 dark:text-gray-500 italic">Analysis not yet available.</p>
{:else}
  <!-- Recommendation pill -->
  <div class="mb-3 inline-flex items-center gap-2 rounded border px-3 py-1.5 text-sm font-semibold {recColour()}">
    {analysis.recommendation}
    <span class="text-xs font-normal {confColour()}">({analysis.confidence} confidence)</span>
  </div>

  <!-- Breaking changes -->
  {#if analysis.breaking_changes?.length}
    <div class="mb-2">
      <h4 class="text-xs font-semibold text-red-700 dark:text-red-400 mb-1 uppercase tracking-wide">Breaking changes</h4>
      <ul class="list-disc list-inside space-y-0.5">
        {#each analysis.breaking_changes as bc}
          <li class="text-xs text-gray-700 dark:text-gray-300">{bc}</li>
        {/each}
      </ul>
    </div>
  {/if}

  <!-- Deprecations -->
  {#if analysis.deprecations?.length}
    <div class="mb-2">
      <h4 class="text-xs font-semibold text-amber-700 dark:text-amber-400 mb-1 uppercase tracking-wide">Deprecations</h4>
      <ul class="list-disc list-inside space-y-0.5">
        {#each analysis.deprecations as dep}
          <li class="text-xs text-gray-700 dark:text-gray-300">{dep}</li>
        {/each}
      </ul>
    </div>
  {/if}

  <!-- Codebase impact -->
  {#if analysis.codebase_impact?.length}
    <div class="mb-2">
      <h4 class="text-xs font-semibold text-gray-600 dark:text-gray-400 mb-1 uppercase tracking-wide">Codebase impact</h4>
      <ul class="space-y-0.5">
        {#each analysis.codebase_impact as imp}
          <li class="text-xs">
            <span class="font-mono text-gray-700 dark:text-gray-300">{imp.file}</span>
            {#if imp.impact}
              <span class="text-gray-500 dark:text-gray-400"> — {imp.impact}</span>
            {/if}
          </li>
        {/each}
      </ul>
    </div>
  {/if}

  <!-- Suggested code changes -->
  {#if analysis.code_changes?.length}
    <div class="mb-2">
      <h4 class="text-xs font-semibold text-gray-600 dark:text-gray-400 mb-1 uppercase tracking-wide">Suggested changes</h4>
      <ul class="space-y-0.5">
        {#each analysis.code_changes as ch}
          <li class="text-xs">
            <span class="font-mono text-gray-700 dark:text-gray-300">{ch.file}</span>
            <span class="text-gray-500 dark:text-gray-400"> — {ch.description}</span>
          </li>
        {/each}
      </ul>
    </div>
  {/if}

  <!-- Full review body (collapsible) -->
  {#if analysis.review_body}
    <details class="mt-2">
      <summary class="text-xs text-blue-500 dark:text-blue-400 cursor-pointer hover:underline select-none">
        Full review body
      </summary>
      <pre class="mt-1 text-xs text-gray-700 dark:text-gray-300 whitespace-pre-wrap break-words rounded bg-gray-50 dark:bg-gray-900 p-2">{analysis.review_body}</pre>
    </details>
  {/if}
{/if}
