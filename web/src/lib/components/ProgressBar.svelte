<script lang="ts">
  // null draws the indeterminate bar, for a phase that can't be measured yet.
  let { value, label, class: cls = '' }: { value: number | null; label?: string; class?: string } = $props();
</script>

{#if value === null}
  <progress class={`progress progress-primary ${cls}`} aria-label={label}></progress>
{:else}
  <progress class={`progress progress-primary ${cls}`} aria-label={label} {value} max="100"></progress>
{/if}
