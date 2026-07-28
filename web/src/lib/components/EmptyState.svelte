<script lang="ts">
  import type { Snippet } from 'svelte';
  import Icon from './Icon.svelte';
  import type { IconName } from './icons';

  let {
    icon = 'info',
    title,
    message = '',
    tone = 'muted',
    children,
  }: {
    icon?: IconName;
    title: string;
    message?: string;
    tone?: 'muted' | 'danger';
    children?: Snippet;
  } = $props();
</script>

<div class="card bg-base-100 shadow-sm">
  <div class="card-body items-center gap-3 py-12 text-center">
    <span
      class={`flex h-12 w-12 items-center justify-center rounded-box ${
        tone === 'danger' ? 'bg-error/10 text-error' : 'bg-primary/10 text-primary'
      }`}
    >
      <Icon name={icon} size={24} />
    </span>
    <div>
      <p class="text-base font-semibold">{title}</p>
      {#if message}
        <p class="mx-auto mt-1 max-w-md text-sm text-base-content/60">{message}</p>
      {/if}
    </div>
    {#if children}
      <div class="mt-1">{@render children()}</div>
    {/if}
  </div>
</div>
