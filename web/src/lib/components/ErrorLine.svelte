<script lang="ts">
  // Renders nothing while error is null, so callers just pass their error state.
  import Icon from './Icon.svelte';

  let {
    error,
    size = 'sm',
    variant = 'line',
    className = '',
  }: {
    error: string | null;
    size?: 'sm' | 'xs';
    variant?: 'line' | 'alert';
    className?: string;
  } = $props();
</script>

{#if error && variant === 'alert'}
  <div role="alert" class={`alert alert-error alert-soft ${className}`}>
    <Icon name="alert" size={16} />
    <span>{error}</span>
  </div>
{:else if error}
  <p class={`${className} flex items-center gap-1.5 text-error ${size === 'xs' ? 'text-xs' : 'text-sm'}`}>
    <Icon name="alert" size={size === 'xs' ? 13 : 14} stroke={2} />
    {error}
  </p>
{/if}
