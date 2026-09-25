<script lang="ts">
  // Standard inline form-error line (icon + message); renders nothing while
  // error is null, so callers just pass their error state.
  import Icon from './Icon.svelte';

  let {
    error,
    size = 'sm',
    className = '',
  }: {
    error: string | null;
    size?: 'sm' | 'xs';
    className?: string;
  } = $props();
</script>

{#if error}
  <p class={`${className} flex items-center gap-1.5 text-error ${size === 'xs' ? 'text-xs' : 'text-sm'}`}>
    <Icon name="alert" size={size === 'xs' ? 13 : 14} stroke={2} />
    {error}
  </p>
{/if}
