<script lang="ts">
  // Standard inline form-error line (icon + catalogue text); renders nothing
  // while failure is null, so callers just bind their ErrorRef state.
  import { errorRefText, type ErrorRef } from '../error-text';
  import Icon from './Icon.svelte';

  let {
    failure,
    size = 'sm',
    className = '',
  }: {
    failure: ErrorRef | null;
    size?: 'sm' | 'xs';
    className?: string;
  } = $props();
</script>

{#if failure}
  <p class={`${className} flex items-center gap-1.5 text-error ${size === 'xs' ? 'text-xs' : 'text-sm'}`}>
    <Icon name="alert" size={size === 'xs' ? 13 : 14} stroke={2} />
    {errorRefText(failure)}
  </p>
{/if}
