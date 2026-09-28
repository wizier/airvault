<script lang="ts">
  import { errMsg } from '../api/client';
  import { powerDevice, type Device } from '../api/devices';
  import { deviceFileSource } from '../api/files';
  import { blockedReason, connectionUi, modelDisplayName, osName } from '../device-ui';
  import { liveRun } from '../events.svelte';
  import { formatDateTime, relativeTime } from '../format';
  import { now } from '../clock';
  import { autoDismiss } from '../timers.svelte';
  import AppsModal from './AppsModal.svelte';
  import Badge from './Badge.svelte';
  import ConfirmDialog from './ConfirmDialog.svelte';
  import ConsoleModal from './ConsoleModal.svelte';
  import DeviceActions from './DeviceActions.svelte';
  import DeviceBattery from './DeviceBattery.svelte';
  import DeviceFrame from './DeviceFrame.svelte';
  import FileBrowser from './FileBrowser.svelte';
  import Gallery from './Gallery.svelte';
  import Icon from './Icon.svelte';
  import Pill from './Pill.svelte';

  let { device }: { device: Device } = $props();

  const conn = $derived(connectionUi(device.connection));
  const reachable = $derived(device.connection !== 'offline');
  const live = $derived(liveRun(device.udid));
  const restoreRunning = $derived(live?.restore ?? false);
  const powerBlocked = $derived(blockedReason(device, live));
  const mediaFiles = $derived(deviceFileSource(device.udid));

  let consoleOpen = $state(false);
  let appsOpen = $state(false);
  let filesOpen = $state(false);
  let galleryOpen = $state(false);
  let powerAsked = $state<'restart' | 'shutdown' | null>(null);
  let controlNotice = $state<{ tone: 'info' | 'error'; text: string } | null>(null);
  autoDismiss(() => controlNotice, () => (controlNotice = null));

  async function power(action: 'restart' | 'shutdown') {
    await powerDevice(device.udid, action);
    controlNotice = {
      tone: 'info',
      text:
        action === 'restart'
          ? 'Restarting — the phone drops offline and comes back in a couple of minutes'
          : 'Shutting down — plug it into power or a computer to turn it back on',
    };
  }

  async function sleepDevice() {
    (document.activeElement as HTMLElement | null)?.blur();
    try {
      await powerDevice(device.udid, 'sleep');
      controlNotice = { tone: 'info', text: 'Asked the phone to sleep' };
    } catch (error) {
      controlNotice = { tone: 'error', text: errMsg(error, 'sleep_request_failed') };
    }
  }

</script>

<div class="card bg-base-100 shadow-sm">
  <div class="card-body gap-4 p-5 sm:p-6">
    <div class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-3 sm:grid-rows-[1fr_auto]">
      <div class="sm:row-span-2">
        <DeviceFrame
          udid={device.udid}
          productType={device.productType}
          connection={device.connection}
          lockScreen={device.lockScreen}
        />
      </div>
        <div class="flex min-w-0 items-start gap-3">
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <h1 class="truncate text-xl font-semibold tracking-tight sm:text-2xl" title={device.name}>{device.name}</h1>
              <DeviceBattery {device} badge />
              <Pill tone={conn.tone}><Icon name={conn.icon} size={12} stroke={2} />{conn.label}</Pill>
            </div>
            <p class="mt-1.5 flex flex-wrap items-center gap-x-2 text-sm text-base-content/60">
              {#if device.productType}<span title={device.productType}>{modelDisplayName(device.productType)}</span><span class="text-base-content/30">·</span>{/if}
              {#if device.iosVersion}<span>{osName(device.productType, device.iosVersion)} {device.iosVersion}</span><span class="text-base-content/30">·</span>{/if}
              <span class="select-all break-all font-mono text-xs" title="UDID">{device.udid}</span>
            </p>
            <div class="mt-1.5">
              <Badge label="Paired" offLabel="Not paired" icon="shield" on={device.paired} />
            </div>
            {#if device.connection === 'offline' && device.lastSeen}
              <p class="mt-1 text-xs text-base-content/50" title={formatDateTime(device.lastSeen)}>
                Last seen {relativeTime(device.lastSeen, $now)}
              </p>
            {/if}
          </div>
          <DeviceActions {device} />
        </div>

        <!-- Direct controls: own full-width row on phones, share the phone's bottom edge from sm up. -->
        <div class="col-span-2 flex flex-wrap items-center justify-end gap-1 self-end sm:col-span-1 sm:col-start-2">
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            disabled={!reachable}
            onclick={() => (consoleOpen = true)}
            title={!reachable ? 'Device is offline' : 'Open the live system log'}
          >
            <Icon name="terminal" size={14} /> Console
          </button>
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            disabled={!reachable}
            onclick={() => (appsOpen = true)}
            title={!reachable ? 'Device is offline' : 'List installed applications'}
          >
            <Icon name="apps" size={14} /> Apps
          </button>
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            disabled={!reachable || restoreRunning}
            onclick={() => (filesOpen = true)}
            title={!reachable ? 'Device is offline' : restoreRunning ? 'A restore is running' : 'Browse files on the device'}
          >
            <Icon name="folder" size={14} /> Files
          </button>
          <button
            type="button"
            class="btn btn-ghost btn-sm"
            disabled={!reachable || restoreRunning}
            onclick={() => (galleryOpen = true)}
            title={!reachable ? 'Device is offline' : restoreRunning ? 'A restore is running' : 'Browse the camera roll'}
          >
            <Icon name="image" size={14} /> Media
          </button>
          {#if powerBlocked}
            <button type="button" class="btn btn-ghost btn-sm" disabled title={powerBlocked}>
              <Icon name="power" size={14} /> Power
            </button>
          {:else}
            <div class="dropdown dropdown-end">
              <div tabindex="0" role="button" class="btn btn-ghost btn-sm" title="Power actions">
                <Icon name="power" size={14} /> Power
              </div>
              <ul class="dropdown-content menu z-10 w-48 gap-0.5 rounded-box bg-base-100 p-2 shadow-lg">
                <li><button type="button" onclick={() => (powerAsked = 'restart')}>Restart…</button></li>
                <li><button type="button" onclick={sleepDevice}>Sleep</button></li>
                <li>
                  <button type="button" class="text-error" onclick={() => (powerAsked = 'shutdown')}>Shut down…</button>
                </li>
              </ul>
            </div>
          {/if}
        </div>
    </div>

    {#if controlNotice}
      <p class={`flex items-center gap-1.5 text-xs ${controlNotice.tone === 'error' ? 'text-error' : 'text-info'}`}>
        <span class={`status ${controlNotice.tone === 'error' ? 'status-error' : 'status-info'}`}></span>
        {controlNotice.text}
      </p>
    {/if}
  </div>
</div>

<!-- Rendered only while open so each open is a fresh instance: modal state
     (buffered log, search, notices) resets by remount, not by hand-clearing. -->
{#if consoleOpen}
  <ConsoleModal udid={device.udid} name={device.name} onclose={() => (consoleOpen = false)} />
{/if}
{#if appsOpen}
  <AppsModal udid={device.udid} name={device.name} onclose={() => (appsOpen = false)} />
{/if}
{#if filesOpen}
  <FileBrowser
    source={mediaFiles}
    title={`Files — ${device.name}`}
    subtitle="Photos, videos and files on the device (DCIM, Recordings, …)"
    rootLabel="Files"
    onclose={() => (filesOpen = false)}
  />
{/if}
{#if galleryOpen}
  <Gallery udid={device.udid} name={device.name} onclose={() => (galleryOpen = false)} />
{/if}

{#if powerAsked}
  {@const action = powerAsked}
  {@const restart = action === 'restart'}
  <ConfirmDialog
    title={restart ? `Restart ${device.name}?` : `Shut down ${device.name}?`}
    icon="power"
    confirmLabel={restart ? 'Restart' : 'Shut down'}
    busyLabel="Asking…"
    confirmClass={restart ? 'btn-warning' : 'btn-error'}
    failureCode="power_request_failed"
    onconfirm={() => power(action)}
    onclose={() => (powerAsked = null)}
  >
    <p class="py-3 text-sm text-base-content/70">
      {#if restart}
        The phone reboots immediately and drops offline for a couple of minutes. Unsaved state in open apps may be lost —
        exactly like holding the power button.
      {:else}
        The phone powers off and stays offline until it's turned back on by hand (or plugged into power). Remote backups
        stop until then.
      {/if}
    </p>
  </ConfirmDialog>
{/if}
