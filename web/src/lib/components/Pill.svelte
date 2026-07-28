<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { Tone } from '../device-ui';

  let {
    tone = 'slate',
    dot = false,
    children,
  }: {
    tone?: Tone;
    dot?: boolean;
    children?: Snippet;
  } = $props();

  // Map the shared Tone vocabulary onto daisyUI semantic badge colors so both
  // themes work automatically. Offline/neutral reads as a muted ghost badge.
  const badgeClass: Record<Tone, string> = {
    green: 'badge-soft badge-success',
    amber: 'badge-soft badge-warning',
    red: 'badge-soft badge-error',
    slate: 'badge-ghost',
  };

  const statusClass: Record<Tone, string> = {
    green: 'status-success',
    amber: 'status-warning',
    red: 'status-error',
    slate: 'status-neutral',
  };
</script>

<span class={`badge badge-sm gap-1.5 whitespace-nowrap ${badgeClass[tone]}`}>
  {#if dot}
    <span class={`status ${statusClass[tone]}`}></span>
  {/if}
  {@render children?.()}
</span>
