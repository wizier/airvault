<script lang="ts">
  // SSE health is deliberately not shown: it reconnects on its own, and a dead
  // backend already raises the App banner.
  import { link, push } from 'svelte-spa-router';
  import Logo from './Logo.svelte';
  import Icon from './Icon.svelte';
  import ThemeToggle from './ThemeToggle.svelte';
  import { statusStore } from '../stores.svelte';
  import { eventsClient } from '../events.svelte';
  import { logout } from '../api/session';

  // Mounted on every non-login route: owns the status fetch (bridge badge here,
  // offline banner in App) and the single shared SSE stream.
  $effect(() => statusStore.start());
  $effect(() => eventsClient.start());

  const status = $derived(statusStore.data);

  async function signOut() {
    try {
      await logout();
    } finally {
      push('/login');
    }
  }
</script>

<header class="sticky top-0 z-20 border-b border-base-300 bg-base-100/85 backdrop-blur">
  <div class="navbar mx-auto min-h-14 w-full max-w-6xl gap-2 px-4 sm:px-6">
    <div class="navbar-start min-w-0">
      <a href="/" use:link class="flex items-center gap-2" aria-label="AirVault home">
        <Logo size={36} />
        <span class="text-base font-semibold tracking-tight">AirVault</span>
      </a>
    </div>

    <div class="navbar-end gap-2">
      {#if status}
        <span
          class={`hidden gap-1.5 sm:inline-flex badge badge-soft ${
            status.muxerUp ? 'badge-success' : 'badge-error'
          }`}
          title={status.muxerUp ? 'Device bridge ready' : 'Device bridge unavailable'}
        >
          <span
            class={`status ${status.muxerUp ? 'status-success animate-pulse' : 'status-error'}`}
          ></span>
          device bridge
        </span>
      {/if}
      <ThemeToggle />
      <button
        type="button"
        class="btn btn-ghost btn-circle"
        title="Sign out"
        aria-label="Sign out"
        onclick={signOut}
      >
        <Icon name="logout" size={18} />
      </button>
    </div>
  </div>
</header>
