<script lang="ts">
  // Backups whose phone is no longer registered: kept visible so stored data
  // never lingers unnoticed. A row opens the device page (browse, migrate or
  // delete individual points); the button here removes the whole source.
  import { link } from 'svelte-spa-router';
  import { deleteDeviceBackups } from '../api/backups';
  import { errRef } from '../api/client';
  import type { Device } from '../api/devices';
  import type { ErrorRef } from '../error-text';
  import { devicesStore, restoreSourcesStore } from '../stores.svelte';
  import { formatBytes, formatDateTime, relativeTime, shortUdid } from '../format';
  import { deviceIcon, modelDisplayName } from '../device-ui';
  import { now } from '../clock';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';

  let { orphans }: { orphans: Device[] } = $props();

  let pending = $state<Device | null>(null);
  let deleting = $state(false);
  let failure = $state<ErrorRef | null>(null);
  let dialog = $state<HTMLDialogElement | null>(null);

  function askDelete(orphan: Device) {
    pending = orphan;
    failure = null;
    dialog?.showModal();
  }

  async function confirmDelete() {
    if (!pending || deleting) return;
    const udid = pending.udid;
    deleting = true;
    failure = null;
    try {
      await deleteDeviceBackups(udid);
      devicesStore.mutate((devices) => devices.filter((device) => device.udid !== udid));
      restoreSourcesStore.mutate((sources) => sources.filter((source) => source.udid !== udid));
      dialog?.close();
      pending = null;
    } catch (error) {
      failure = errRef(error, 'device_action_failed');
    } finally {
      deleting = false;
    }
  }
</script>

<section class="flex flex-col gap-3">
  <div>
    <h2 class="text-xs font-semibold uppercase tracking-wide text-base-content/50">
      Backups without a phone
    </h2>
    <p class="mt-1 text-xs text-base-content/60">
      These phones are no longer registered here, but their backups are still stored. Keep them to
      migrate onto a new phone, or delete them to free the space.
    </p>
  </div>
  <div class="card bg-base-100 shadow-sm">
    <ul class="divide-y divide-base-300">
      {#each orphans as orphan (orphan.udid)}
        {@const named = orphan.name !== orphan.udid}
        <li class="flex flex-wrap items-center justify-between gap-3 p-4">
          <a
            use:link
            href={`/device/${encodeURIComponent(orphan.udid)}`}
            class="flex min-w-0 items-center gap-3"
            title="Open the stored backups of this phone"
          >
            <span class="flex h-9 w-9 shrink-0 items-center justify-center rounded-box bg-base-200 text-base-content/60">
              <Icon name={deviceIcon(orphan.productType)} size={17} />
            </span>
            <span class="min-w-0">
              {#if named}
                <span class="block truncate text-sm font-medium" title={orphan.name}>{orphan.name}</span>
              {:else}
                <span class="block truncate font-mono text-sm" title={orphan.udid}>{shortUdid(orphan.udid)}</span>
              {/if}
              <span class="mt-0.5 block text-xs text-base-content/60">
                {#if named}
                  {#if orphan.productType}{modelDisplayName(orphan.productType)} · {/if}<span class="font-mono" title={orphan.udid}>{shortUdid(orphan.udid)}</span> ·
                {/if}
                {orphan.restorePoints || 0} restore point{orphan.restorePoints === 1 ? '' : 's'}
                {#if orphan.diskBytes !== undefined}· {formatBytes(orphan.diskBytes)}{/if}
                {#if orphan.lastBackup}
                  · backed up
                  <span title={formatDateTime(orphan.lastBackup)}>{relativeTime(orphan.lastBackup, $now)}</span>
                {/if}
              </span>
            </span>
          </a>
          <button
            type="button"
            class="btn btn-error btn-outline btn-xs"
            onclick={() => askDelete(orphan)}
          >
            <Icon name="trash" size={13} />
            Delete…
          </button>
        </li>
      {/each}
    </ul>
  </div>
</section>

<dialog
  class="modal"
  bind:this={dialog}
  oncancel={(event) => deleting && event.preventDefault()}
  onclose={() => (pending = null)}
>
  <div class="modal-box">
    <h3 class="text-lg font-bold">
      Delete the backups of {pending ? (pending.name !== pending.udid ? pending.name : shortUdid(pending.udid)) : 'this phone'}?
    </h3>
    <p class="py-3 text-sm text-base-content/70">
      {#if pending}
        {pending.restorePoints === 1 ? 'The only restore point' : `All ${pending.restorePoints} restore points`}
        of this phone will be removed{#if pending.diskBytes !== undefined}, freeing about
          <span class="font-medium tabular-nums">{formatBytes(pending.diskBytes)}</span> on disk{/if}.
        This can't be undone.
      {/if}
    </p>
    <ErrorLine {failure} className="mt-3" />
    <div class="modal-action">
      <button type="button" class="btn btn-ghost" disabled={deleting} onclick={() => dialog?.close()}>Keep them</button>
      <button type="button" class="btn btn-error" disabled={deleting} onclick={confirmDelete}>
        {#if deleting}
          <span class="loading loading-spinner loading-xs"></span> Deleting…
        {:else}
          <Icon name="trash" size={15} /> Delete backups
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close" disabled={deleting}>close</button>
  </form>
</dialog>
