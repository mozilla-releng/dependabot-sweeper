<script lang="ts">
  import type { PRStage } from './api'
  import { STAGE_COLOURS, DEFAULT_COLOUR } from './stages'

  let { stage }: { stage: PRStage } = $props()

  const cls = $derived(STAGE_COLOURS[stage] ?? DEFAULT_COLOUR)
</script>

<span class="inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-semibold {cls}">
  {stage}
</span>
