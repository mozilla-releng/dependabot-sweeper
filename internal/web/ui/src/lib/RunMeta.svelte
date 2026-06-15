<script lang="ts">
  import type { PRProgress } from './api'

  let { pr }: { pr: PRProgress } = $props()

  function fmtTime(s: string | undefined): string {
    if (!s || s.startsWith('0001-01-01')) return '—'
    const d = new Date(s)
    return isNaN(d.getTime()) ? '—' : d.toLocaleString()
  }

  type Row = { label: string; value: string; mono?: boolean; href?: string }
  const rows = $derived<Row[]>([
    { label: 'Last updated', value: fmtTime(pr.last_updated) },
    ...(pr.impl_branch   ? [{ label: 'Branch',          value: pr.impl_branch,        mono: true }] : []),
    ...(pr.worktree_path ? [{ label: 'Worktree',         value: pr.worktree_path,      mono: true }] : []),
    ...(pr.session_id    ? [{ label: 'Session',          value: pr.session_id,         mono: true }] : []),
    ...(pr.replacement_pr ? [{ label: 'Replacement PR',  value: `#${pr.replacement_pr}`, href: pr.replacement_pr_url }] : []),
  ])
</script>

{#if rows.length === 0}
  <p class="text-xs text-gray-400 dark:text-gray-500 italic">No metadata yet.</p>
{:else}
  <dl class="space-y-1">
    {#each rows as row}
      <div class="flex items-start gap-2 text-xs">
        <dt class="w-28 shrink-0 text-gray-500 dark:text-gray-400">{row.label}</dt>
        <dd class="flex-1 min-w-0 text-gray-800 dark:text-gray-200 truncate {row.mono ? 'font-mono' : ''}"
            title={row.value}
        >{#if row.href}<a href={row.href} target="_blank" rel="noopener noreferrer" class="hover:underline">{row.value}</a>{:else}{row.value}{/if}</dd>
      </div>
    {/each}
  </dl>
{/if}
