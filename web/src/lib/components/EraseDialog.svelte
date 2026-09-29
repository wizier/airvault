<script lang="ts">
  // Mounted per open, so Find My is read fresh each time.
  import type { Device } from '../api/devices';
  import { now } from '../clock';
  import { formatDateTime, relativeTime } from '../format';
  import { erase, hardwareResources } from '../stores.svelte';
  import ConfirmDialog from './ConfirmDialog.svelte';
  import FindMyAlert from './FindMyAlert.svelte';
  import Icon from './Icon.svelte';

  let { device, onclose }: { device: Device; onclose: () => void } = $props();

  // An erased phone with Find My on stays locked to its owner. An unknown state
  // never blocks: the server checks again.
  const hwRes = $derived(hardwareResources.for(device.udid));
  const findMyOn = $derived(hwRes.data?.findMyEnabled === true);
  // Erasing is confirmed by typing the phone's name, as it can't be undone.
  let typedName = $state('');

  $effect(() => {
    void hwRes.refresh();
    return hwRes.start();
  });
</script>

<ConfirmDialog
  title={`Erase ${device.name}?`}
  icon="alert"
  confirmLabel="Erase iPhone"
  busyLabel="Erasing…"
  confirmDisabled={findMyOn || typedName.trim() !== device.name}
  failureCode="erase_failed"
  onconfirm={() => erase(device.udid)}
  {onclose}
>
  <div class="flex flex-col gap-3 py-3 text-sm text-base-content/70">
    <p>
      Erases all content and settings, as Settings → General → Transfer or Reset → Erase All Content and
      Settings does, and restarts the phone into setup. The phone may ask for its passcode first. It then leaves
      AirVault; to use it here again, pair it over USB from scratch.
    </p>
    {#if device.lastBackup}
      <p>
        Its backups stay in AirVault and can be restored onto any phone. The last one was made
        <span class="font-medium text-base-content/80" title={formatDateTime(device.lastBackup)}>{relativeTime(device.lastBackup, $now)}</span>.
      </p>
    {:else}
      <div role="alert" class="alert alert-error alert-soft">
        <Icon name="alert" size={18} />
        <div>
          <p class="font-medium">This phone has no backup</p>
          <p class="mt-1 opacity-80">Everything on it will be lost. Back it up first.</p>
        </div>
      </div>
    {/if}
    {#if findMyOn}
      <FindMyAlert udid={device.udid} />
    {:else}
      <label class="flex flex-col gap-1.5">
        <span>Type <span class="font-medium text-base-content">{device.name}</span> to confirm</span>
        <input type="text" class="input input-sm w-full" autocomplete="off" bind:value={typedName} />
      </label>
    {/if}
  </div>
</ConfirmDialog>
