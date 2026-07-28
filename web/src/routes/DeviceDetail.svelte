<script lang="ts">
  // Route boundary only: resolve the requested device and compose the feature
  // cards. Each card owns its requests, mutations, modals and transient state.
  import { link, push } from 'svelte-spa-router';
  import { errMsg } from '../lib/api/client';
  import BackupCard from '../lib/components/BackupCard.svelte';
  import BackupHistory from '../lib/components/BackupHistory.svelte';
  import DeviceOverviewCard from '../lib/components/DeviceOverviewCard.svelte';
  import EmptyState from '../lib/components/EmptyState.svelte';
  import HardwareCard from '../lib/components/HardwareCard.svelte';
  import Icon from '../lib/components/Icon.svelte';
  import { devicesStore } from '../lib/stores.svelte';

  let { params }: { params: { udid: string } } = $props();
  const udid = $derived(params.udid);
  // The history's restore shortcut calls into the backup card's modal.
  let backupCard = $state<ReturnType<typeof BackupCard> | null>(null);

  $effect(() => devicesStore.start());

  const device = $derived((devicesStore.data ?? []).find((candidate) => candidate.udid === udid) ?? null);
  // A device absent from the loaded list (unpaired — here or elsewhere — or a
  // stale URL) sends the viewer to the dashboard; there is no not-found page.
  $effect(() => {
    if (devicesStore.ready && !device) void push('/');
  });
  const loadError = $derived(
    !devicesStore.ready && devicesStore.error && !devicesStore.offline
      ? errMsg(devicesStore.error, 'device_load_failed')
      : null,
  );
</script>

<div class="flex flex-col gap-6">
  <div>
    <a href="/" use:link class="btn btn-ghost btn-sm -ml-2">
      <Icon name="arrowLeft" size={15} />
      Devices
    </a>
  </div>

  {#if device}
    <DeviceOverviewCard {device} />
    <HardwareCard {udid} reachable={device.connection !== 'offline'} />
    <BackupCard {device} bind:this={backupCard} />
    <BackupHistory {device} onrestore={(snapshotId) => backupCard?.openRestore(snapshotId)} />
  {:else if loadError}
    <EmptyState icon="alert" tone="danger" title="Could not load device" message={loadError}>
      <button type="button" class="btn btn-primary btn-sm" onclick={() => devicesStore.refresh()}>Retry</button>
    </EmptyState>
  {:else}
    <div class="flex items-center gap-3 text-sm text-base-content/60">
      <span class="loading loading-spinner loading-sm"></span>
      Loading device…
    </div>
  {/if}
</div>
