<script lang="ts">
  import type { StageEvent } from './api'
  import { STAGE_COLOURS, DEFAULT_COLOUR } from './stages'

  let { history }: { history: StageEvent[] } = $props()

  function fmtTime(s: string): string {
    if (!s || s.startsWith('0001-01-01')) return ''
    const d = new Date(s)
    return isNaN(d.getTime()) ? '' : d.toLocaleTimeString()
  }

  // Compute elapsed between consecutive events.
  function elapsedLabel(i: number): string {
    if (i === 0) return ''
    const prev = new Date(history[i - 1].at).getTime()
    const curr = new Date(history[i].at).getTime()
    if (isNaN(prev) || isNaN(curr)) return ''
    const ms = curr - prev
    if (ms < 1000) return `+${ms}ms`
    if (ms < 60_000) return `+${(ms / 1000).toFixed(1)}s`
    return `+${(ms / 60_000).toFixed(1)}m`
  }
</script>

<div class="space-y-1">
  {#each history as ev, i}
    <div class="flex items-start gap-2 text-xs">
      <!-- dot + vertical line -->
      <div class="flex flex-col items-center">
        <span class="mt-0.5 h-2 w-2 rounded-full shrink-0 {STAGE_COLOURS[ev.stage] ? 'ring-1 ring-current ' + STAGE_COLOURS[ev.stage] : DEFAULT_COLOUR}"></span>
        {#if i < history.length - 1}
          <span class="w-px flex-1 bg-gray-200 dark:bg-gray-700 my-0.5"></span>
        {/if}
      </div>
      <!-- content -->
      <div class="flex-1 min-w-0 pb-1">
        <div class="flex items-baseline gap-1.5 flex-wrap">
          <span class="font-semibold text-gray-800 dark:text-gray-200">{ev.stage}</span>
          {#if elapsedLabel(i)}
            <span class="text-gray-400 dark:text-gray-500 tabular-nums">{elapsedLabel(i)}</span>
          {/if}
          {#if fmtTime(ev.at)}
            <span class="text-gray-400 dark:text-gray-500 tabular-nums ml-auto">{fmtTime(ev.at)}</span>
          {/if}
        </div>
        {#if ev.detail}
          <p class="text-gray-500 dark:text-gray-400 truncate" title={ev.detail}>{ev.detail}</p>
        {/if}
      </div>
    </div>
  {/each}

  {#if !history?.length}
    <p class="text-xs text-gray-400 dark:text-gray-500 italic">No history yet.</p>
  {/if}
</div>
