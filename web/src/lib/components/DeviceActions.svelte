<script lang="ts">
  // Device management menu (kebab) + confirmation modals. Two destructive
  // actions by design: Unpair (THE "remove device") and Delete backups (the
  // device stays registered).
  import { deleteDeviceBackups } from '../api/backups';
  import { errRef } from '../api/client';
  import { unpairDevice, type Device } from '../api/devices';
  import type { ErrorRef } from '../error-text';
  import {
    devicesStore,
    restoreSourcesStore,
    restorePointResources,
  } from '../stores.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';

  let { device }: { device: Device } = $props();

  type Action = 'unpair' | 'deleteBackups';
  let dialog = $state<HTMLDialogElement | null>(null);
  let action = $state<Action>('unpair');
  let alsoDelete = $state(false);
  let busy = $state(false);
  let failure = $state<ErrorRef | null>(null);

  function open(a: Action) {
    action = a;
    alsoDelete = false;
    failure = null;
    dialog?.showModal(); // moving focus here also closes the dropdown
  }

  function cancel() {
    if (busy) return;
    dialog?.close();
  }

  async function confirm() {
    if (busy) return;
    busy = true;
    failure = null;
    try {
      if (action === 'unpair') {
        await unpairDevice(device.udid, { deleteBackups: alsoDelete });
        // Drop it now; the device.removed SSE confirms moments later, idempotently.
        devicesStore.mutate((list) => list.filter((d) => d.udid !== device.udid));
        dialog?.close();
      } else {
        await deleteDeviceBackups(device.udid);
        restorePointResources.mutate(device.udid, () => []);
        restoreSourcesStore.mutate((sources) =>
          sources.filter((source) => source.udid !== device.udid),
        );
        devicesStore.mutate((devices) =>
          devices.map((current) =>
            current.udid === device.udid
              ? {
                  ...current,
                  diskBytes: undefined,
                  restorePoints: undefined,
                  lastBackup: undefined,
                  lastRunErrors: undefined,
                }
              : current,
          ),
        );
        dialog?.close();
      }
    } catch (err) {
      failure = errRef(err, 'device_action_failed');
    } finally {
      busy = false;
    }
  }
</script>

<div class="dropdown dropdown-end">
  <div tabindex="0" role="button" class="btn btn-square btn-ghost" aria-label="Device actions">
    <Icon name="more" size={18} />
  </div>
  <ul class="dropdown-content menu z-10 w-60 gap-0.5 rounded-box bg-base-100 p-2 shadow-lg">
    <li>
      <button type="button" onclick={() => open('deleteBackups')}>
        <Icon name="trash" size={15} />
        Delete backups…
      </button>
    </li>
    <li>
      <button type="button" class="text-error" onclick={() => open('unpair')}>
        <Icon name="offline" size={15} />
        Unpair device…
      </button>
    </li>
  </ul>
</div>

<dialog class="modal" bind:this={dialog}>
  <div class="modal-box">
    {#if action === 'unpair'}
      <h3 class="text-lg font-bold">Unpair {device.name}?</h3>
      <p class="py-3 text-sm text-base-content/70">
        The iPhone is told to forget this AirVault host, and the device is removed from AirVault
        either way. If it can't be reached right now, clear the trust on the phone later via
        Settings → General → Transfer or Reset → Reset Location &amp; Privacy. To back it up
        again, you'll pair over USB from scratch.
      </p>
      <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
        <input type="checkbox" class="checkbox checkbox-sm checkbox-error" bind:checked={alsoDelete} />
        <span class="text-sm">
          Also delete this device's stored backups
          <span class="text-base-content/60">(can't be undone)</span>
        </span>
      </label>
    {:else}
      <h3 class="text-lg font-bold">Delete backups of {device.name}?</h3>
      <p class="py-3 text-sm text-base-content/70">
        Deletes all restore points for this device. The device stays in AirVault and can be backed up
        again manually. This can't be undone.
      </p>
    {/if}

    <ErrorLine {failure} className="mt-3" />

    <div class="modal-action">
      <button type="button" class="btn btn-ghost" disabled={busy} onclick={cancel}>Cancel</button>
      <button type="button" class="btn btn-error" disabled={busy} onclick={() => confirm()}>
        {#if busy}
          <span class="loading loading-spinner loading-xs"></span>
          Working…
        {:else if action === 'unpair'}
          <Icon name="offline" size={15} />
          Unpair
        {:else}
          <Icon name="trash" size={15} />
          Delete backups
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
