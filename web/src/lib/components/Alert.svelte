<script lang="ts">
  // With a title the content is its dimmed detail line; without one, the
  // content is the message itself.
  import type { Snippet } from 'svelte';
  import Icon from './Icon.svelte';
  import type { IconName } from './icons';

  type AlertTone = 'info' | 'success' | 'warning' | 'error';

  let {
    tone,
    icon = 'alert',
    title,
    class: cls = '',
    children,
    actions,
  }: {
    tone: AlertTone;
    icon?: IconName;
    title?: string;
    class?: string;
    children?: Snippet;
    actions?: Snippet;
  } = $props();

  const toneClass: Record<AlertTone, string> = {
    info: 'alert-info',
    success: 'alert-success',
    warning: 'alert-warning',
    error: 'alert-error',
  };
</script>

<div role={tone === 'success' ? 'status' : 'alert'} class={`alert alert-soft ${toneClass[tone]} ${cls}`}>
  <Icon name={icon} size={18} />
  <div>
    {#if title}
      <p class="font-medium">{title}</p>
      {#if children}<div class="mt-1 text-sm opacity-80">{@render children()}</div>{/if}
    {:else}
      {@render children?.()}
    {/if}
  </div>
  {@render actions?.()}
</div>
