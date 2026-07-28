<script lang="ts">
  // In-flight download indicator: a percent bar (or spinner before the first SSE
  // frame) plus a cancel button. Presentational — the caller owns the Downloads
  // instance and gates this with {#if downloads.active(key)}.
  import Icon from './Icon.svelte';

  let {
    percent,
    oncancel,
    size = 'xs',
  }: {
    percent: number | null;
    oncancel: () => void;
    size?: 'xs' | 'sm';
  } = $props();
</script>

<div class="flex shrink-0 items-center gap-1.5">
  {#if percent != null}
    <progress
      class="progress progress-primary h-1 {size === 'sm' ? 'w-20' : 'w-16'}"
      value={percent}
      max="100"
    ></progress>
    <span class="text-xs tabular-nums text-base-content/60">{percent}%</span>
  {:else}
    <span class="loading loading-spinner loading-xs"></span>
  {/if}
  <button
    type="button"
    class="btn btn-square btn-ghost {size === 'sm' ? 'btn-sm' : 'btn-xs'}"
    title="Cancel download"
    aria-label="Cancel download"
    onclick={oncancel}
  >
    <Icon name="x" size={size === 'sm' ? 16 : 14} />
  </button>
</div>
