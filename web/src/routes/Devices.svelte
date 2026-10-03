<script lang="ts">
  import { statusStore, devicesStore } from '../lib/stores.svelte';
  import Alert from '../lib/components/Alert.svelte';
  import EmptyState from '../lib/components/EmptyState.svelte';
  import DeviceTile from '../lib/components/DeviceTile.svelte';
  import OrphanBackups from '../lib/components/OrphanBackups.svelte';
  import PairWizard from '../lib/components/PairWizard.svelte';
  import Icon from '../lib/components/Icon.svelte';

  // Ref-counted subscribe (Nav holds statusStore); SSE keeps it fresh while mounted.
  $effect(() => devicesStore.start());

  const devices = $derived(devicesStore.data ?? []);
  // Orphaned sources (backups whose phone is gone) get their own section
  // below the grid instead of masquerading as offline phones.
  const phones = $derived(devices.filter((device) => !device.orphaned));
  const orphans = $derived(devices.filter((device) => device.orphaned));
  const showOffline = $derived(devicesStore.offline && !devicesStore.ready);
  const loadError = $derived(devicesStore.loadError('devices_load_failed'));
  const showEmpty = $derived(devicesStore.ready && phones.length === 0);
  // Exception-only: daemon is fine but the device muxer under it is not
  // (global backend unreachability is App.svelte's job).
  const muxer = $derived(statusStore.data?.muxer);
  const muxerDown = $derived(!!muxer && !muxer.up);

  let pairOpen = $state(false);
</script>

<div class="flex flex-col gap-6">
  <div class="flex flex-wrap items-end justify-between gap-3">
    <div>
      <h1 class="text-xl font-semibold tracking-tight sm:text-2xl">Devices</h1>
      <p class="mt-1 text-sm text-base-content/60">Wireless iPhone backups to this NAS</p>
    </div>
    <button type="button" class="btn btn-primary btn-sm" onclick={() => (pairOpen = true)}>
      <Icon name="plug" size={14} />
      Pair device
    </button>
  </div>

  {#if muxerDown}
    <Alert tone="warning" title="USB/Wi-Fi is unavailable">
      netmuxd isn't answering, so devices can't be reached. AirVault reconnects on its own.
      {#if muxer?.error}
        <p class="mt-1 font-mono text-xs opacity-75">{muxer.error}</p>
      {/if}
    </Alert>
  {/if}

  {#if showOffline}
    <EmptyState
      icon="offline"
      tone="danger"
      title="Backend offline"
      message="AirVault can't be reached right now. This view will recover automatically once the daemon is back."
    />
  {:else if loadError}
    <EmptyState icon="alert" tone="danger" title="Could not load devices" message={loadError}>
      <button type="button" class="btn btn-primary btn-sm" onclick={() => devicesStore.refresh()}>Retry</button>
    </EmptyState>
  {:else if showEmpty}
    <EmptyState
      icon="plug"
      title="No devices yet"
      message="Pair an iPhone over USB once to enable wireless backups. It only takes a minute."
    >
      <button type="button" class="btn btn-primary btn-sm" onclick={() => (pairOpen = true)}>
        <Icon name="plug" size={14} />
        Start pairing
      </button>
    </EmptyState>
  {:else}
    <div class="grid grid-cols-1 gap-5 sm:grid-cols-2 xl:grid-cols-3">
      {#each phones as device (device.udid)}
        <DeviceTile {device} />
      {/each}
    </div>
  {/if}

  {#if devicesStore.ready && orphans.length > 0}
    <OrphanBackups {orphans} />
  {/if}
</div>

{#if pairOpen}
  <PairWizard onclose={() => (pairOpen = false)} />
{/if}
