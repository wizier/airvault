<script lang="ts">
  // A person's or a chat's picture, else the letters of its name, as when the
  // picture does not load.
  import { initials } from '../format';

  let { src, name, class: cls = 'w-9' }: { src?: string; name: string; class?: string } = $props();

  let failed = $state<string>();
</script>

{#if src && src !== failed}
  <div class="avatar shrink-0">
    <div class={`rounded-full ${cls}`}><img {src} alt="" loading="lazy" onerror={() => (failed = src)} /></div>
  </div>
{:else}
  <div class="avatar avatar-placeholder shrink-0">
    <div class={`rounded-full bg-base-300 text-base-content/60 ${cls}`}><span class="text-xs">{initials(name)}</span></div>
  </div>
{/if}
