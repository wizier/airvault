<script lang="ts">
  // Device management menu (kebab) + confirmation modals. Two destructive
  // actions by design: Unpair (THE "remove device") and Delete backups (the
  // device stays registered).
  import type { Device } from '../api/devices';
  import { deleteAllBackups, unpair } from '../stores.svelte';
  import ConfirmDialog from './ConfirmDialog.svelte';
  import Icon from './Icon.svelte';

  let { device }: { device: Device } = $props();

  // Opening the dialog moves focus into it, which also closes the dropdown.
  let action = $state<'unpair' | 'deleteBackups' | null>(null);
  let alsoDelete = $state(false);

  function open(next: 'unpair' | 'deleteBackups') {
    action = next;
    alsoDelete = false;
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

{#if action === 'unpair'}
  <ConfirmDialog
    title={`Unpair ${device.name}?`}
    icon="offline"
    confirmLabel="Unpair"
    busyLabel="Working…"
    failureCode="device_action_failed"
    onconfirm={() => unpair(device.udid, alsoDelete)}
    onclose={() => (action = null)}
  >
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
  </ConfirmDialog>
{:else if action === 'deleteBackups'}
  <ConfirmDialog
    title={`Delete backups of ${device.name}?`}
    icon="trash"
    confirmLabel="Delete backups"
    busyLabel="Working…"
    failureCode="device_action_failed"
    onconfirm={() => deleteAllBackups(device)}
    onclose={() => (action = null)}
  >
    <p class="py-3 text-sm text-base-content/70">
      Deletes all restore points for this device. The device stays in AirVault and can be backed up
      again manually. This can't be undone.
    </p>
  </ConfirmDialog>
{/if}
