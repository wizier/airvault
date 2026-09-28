<script lang="ts">
  // A phone leaves usbDevices the moment it pairs; devicesStore then owns its
  // state. The host mounts this only while open, so the pair-state fetch and SSE
  // reactions are scoped to the visible flow.
  import { push } from 'svelte-spa-router';
  import { pairStateStore, devicesStore } from '../stores.svelte';
  import { onPairTrust } from '../events.svelte';
  import { errorCode } from '../api/client';
  import { errorText } from '../error-text';
  import { startTrust as startTrustRequest } from '../api/pairing';
  import Icon from './Icon.svelte';
  import Pill from './Pill.svelte';

  let { onclose }: { onclose: () => void } = $props();

  let dialog: HTMLDialogElement;

  function openDevicePage(udid: string) {
    dialog.close();
    push(`/device/${encodeURIComponent(udid)}`);
  }

  $effect(() => pairStateStore.start());
  $effect(() => devicesStore.start());

  const ps = $derived(pairStateStore.data);
  const usbDevices = $derived(ps?.usbDevices ?? []);
  const muxerOffline = $derived(pairStateStore.ready && ps?.muxerReady === false);
  const backendOffline = $derived(pairStateStore.offline && !pairStateStore.ready);
  const loadError = $derived(pairStateStore.loadError('pair_state_failed'));
  const loading = $derived(!pairStateStore.ready && pairStateStore.error === null);

  let step = $state(1);
  let selectedUdid = $state<string | null>(null);

  // Auto-select the lone USB device. An empty list keeps the selection — the
  // step-2 replug retry still needs its target after an unplug.
  $effect(() => {
    if (!selectedUdid && usbDevices.length === 1) selectedUdid = usbDevices[0].udid;
    if (selectedUdid && step === 1 && usbDevices.length > 0 && !usbDevices.some((d) => d.udid === selectedUdid)) {
      selectedUdid = usbDevices.length === 1 ? usbDevices[0].udid : null;
    }
  });

  const selectedUsb = $derived(usbDevices.find((d) => d.udid === selectedUdid) ?? null);
  // Name source once the phone pairs and leaves usbDevices.
  const registered = $derived(
    (devicesStore.data ?? []).find((d) => d.udid === selectedUdid) ?? null,
  );
  const selectedName = $derived(selectedUsb?.name ?? registered?.name ?? 'your iPhone');

  // A plugged-in phone that's already paired never shows in the pick list; say
  // so instead of a misleading endless "waiting" spinner.
  const pairedOnUsb = $derived(
    (devicesStore.data ?? []).filter((d) => d.connection === 'usb' && d.paired),
  );

  // The trust flow runs SERVER-side: one POST starts it, and every status
  // transition arrives as a pair.trust SSE event (see events.svelte.ts).
  type TrustState =
    | { kind: 'starting' }
    | { kind: 'pending' } // dialog is up on the phone
    | { kind: 'locked' } // phone must be unlocked first
    | { kind: 'denied' } // user tapped "Don't Trust"
    | { kind: 'error'; code: string };
  let trust = $state<TrustState>({ kind: 'starting' });
  const trustError = $derived(trust.kind === 'error' ? errorText(trust.code, 'pairing_failed') : null);
  const wifiAuthorizationFailed = $derived(trust.kind === 'error' && trust.code === 'wifi_authorization_failed');

  async function startTrust(udid: string) {
    trust = { kind: 'starting' };
    try {
      await startTrustRequest(udid);
    } catch (err) {
      trust = { kind: 'error', code: errorCode(err, 'pairing_failed') };
    }
  }

  function goPair() {
    if (!selectedUdid) return;
    step = 2;
    void startTrust(selectedUdid);
  }

  $effect(() =>
    onPairTrust((event) => {
      if (step !== 2 || event.udid !== selectedUdid) return;
      switch (event.status) {
        case 'paired':
          step = 3;
          break;
        case 'trust_pending':
          trust = { kind: 'pending' };
          break;
        case 'locked':
          trust = { kind: 'locked' };
          break;
        case 'denied':
          trust = { kind: 'denied' };
          break;
        default:
          trust = { kind: 'error', code: event.errorCode ?? 'pairing_failed' };
      }
    }),
  );

  // After "Don't Trust", iOS won't show the dialog again on the same USB
  // session — watch presence and restart the flow when the cable is replugged.
  let unplugSeen = false; // non-reactive
  $effect(() => {
    const present = !!selectedUsb;
    if (step !== 2 || trust.kind !== 'denied' || !selectedUdid) {
      unplugSeen = false;
      return;
    }
    if (!present) {
      unplugSeen = true;
    } else if (unplugSeen) {
      unplugSeen = false;
      void startTrust(selectedUdid);
    }
  });

  const stepMeta = [
    { n: 1, label: 'Connect' },
    { n: 2, label: 'Pair' },
    { n: 3, label: 'Done' },
  ];
</script>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} {onclose}>
  <div class="modal-box max-w-2xl">
    <div class="flex flex-col gap-4">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3 class="text-lg font-bold">Pair an iPhone</h3>
          <p class="mt-0.5 text-sm text-base-content/60">
            A one-time USB setup — after it, the phone is reachable over Wi-Fi
          </p>
        </div>
        <button type="button" class="btn btn-square btn-ghost btn-sm" aria-label="Close" onclick={() => dialog.close()}>
          <Icon name="x" size={16} />
        </button>
      </div>

      {#if backendOffline}
        <div role="alert" class="alert alert-error alert-soft">
          <Icon name="offline" size={18} />
          <span>AirVault can't be reached right now — the wizard will start automatically once the daemon is back.</span>
        </div>
      {:else if loadError}
        <div role="alert" class="alert alert-error alert-soft">
          <Icon name="alert" size={18} />
          <div class="flex items-center justify-between gap-3">
            <span>{loadError}</span>
            <button type="button" class="btn btn-ghost btn-sm" onclick={() => pairStateStore.refresh()}>Retry</button>
          </div>
        </div>
      {:else if loading}
        <div class="flex items-center gap-3 rounded-box bg-base-200 p-4 text-sm text-base-content/60">
          <span class="loading loading-spinner loading-sm"></span>
          Connecting to the device bridge…
        </div>
      {:else if muxerOffline}
        <div role="alert" class="alert alert-warning alert-soft">
          <Icon name="alert" size={20} />
          <div>
            <h4 class="font-semibold">Device bridge unavailable</h4>
            <div class="text-sm opacity-90">
              The USB/Wi-Fi device bridge isn't available, so USB devices can't be detected. This screen recovers
              automatically once it's back.
            </div>
          </div>
        </div>
      {:else}
        <ul class="steps w-full">
          {#each stepMeta as m (m.n)}
            <li class={`step ${m.n <= step ? 'step-primary' : ''}`} data-content={m.n < step ? '✓' : undefined}>
              <span class="text-xs">{m.label}</span>
            </li>
          {/each}
        </ul>

        <div class="min-h-52">
          {#if step === 1}
            <div class="flex flex-col gap-4">
              <div>
                <h4 class="text-lg font-semibold">Plug the iPhone into this server</h4>
                <p class="mt-1 text-sm text-base-content/60">
                  iOS requires a cable for the first handshake — this is the only time you'll need one
                </p>
              </div>

              {#if usbDevices.length === 0}
                <div class="flex items-center gap-3 rounded-box bg-base-200 p-4">
                  <span class="loading loading-spinner loading-sm text-primary"></span>
                  <p class="text-sm font-medium">Waiting for a USB device…</p>
                </div>
                {#if pairedOnUsb.length > 0}
                  <div role="alert" class="alert alert-info alert-soft">
                    <Icon name="info" size={18} />
                    <p class="text-sm">
                      <span class="font-medium">{pairedOnUsb[0].name}</span>
                      {pairedOnUsb.length > 1 ? ` and ${pairedOnUsb.length - 1} more are` : ' is'} already
                      paired — manage it from the device list. Only new phones show up here.
                    </p>
                  </div>
                {/if}
                <ul class="ml-4 list-disc space-y-1.5 text-sm text-base-content/60 marker:text-base-content/50">
                  <li>Use a data-capable cable, not a charge-only one.</li>
                  <li>Unlock the phone — locked phones may not show up.</li>
                  <li>Try another USB port if nothing appears.</li>
                </ul>
              {:else if usbDevices.length === 1}
                <div class="flex items-center gap-3 rounded-box bg-base-200 p-4">
                  <span class="flex h-9 w-9 items-center justify-center rounded-box bg-primary/10 text-primary"><Icon name="phone" size={18} /></span>
                  <div class="min-w-0">
                    <p class="truncate text-sm font-semibold">{usbDevices[0].name}</p>
                    <p class="text-xs text-base-content/60">Connected over USB</p>
                  </div>
                  <span class="ml-auto"><Pill tone="green" dot>Ready</Pill></span>
                </div>
              {:else}
                <div>
                  <p class="mb-2 text-sm font-medium">Several devices detected — pick the one to pair:</p>
                  <div class="flex flex-col gap-2">
                    {#each usbDevices as d (d.udid)}
                      <label
                        class={`flex cursor-pointer items-center gap-3 rounded-box border p-3 text-sm transition-colors ${
                          selectedUdid === d.udid ? 'border-primary bg-primary/5' : 'border-base-300 hover:bg-base-200'
                        }`}
                      >
                        <input type="radio" name="usb-device" value={d.udid} bind:group={selectedUdid} class="radio radio-primary radio-sm" />
                        <Icon name="phone" size={16} class="text-primary" />
                        <span class="min-w-0 truncate font-medium">{d.name}</span>
                      </label>
                    {/each}
                  </div>
                </div>
              {/if}

              <div>
                <button type="button" class="btn btn-primary" disabled={!selectedUsb} onclick={goPair}>
                  Continue <Icon name="arrowRight" size={15} />
                </button>
              </div>
            </div>

          {:else if step === 2}
            <div class="flex flex-col gap-4">
              <div>
                {#if wifiAuthorizationFailed}
                  <h4 class="text-lg font-semibold">USB pairing complete</h4>
                  <p class="mt-1 text-sm text-base-content/60">
                    The iPhone already trusts AirVault, so you won't need to approve it again
                  </p>
                {:else}
                  <h4 class="text-lg font-semibold">Tap “Trust” on the iPhone</h4>
                  <p class="mt-1 text-sm text-base-content/60">
                    A “Trust This Computer?” prompt is on the phone. Tap
                    <span class="font-medium text-base-content">Trust</span> and enter the phone's passcode
                  </p>
                {/if}
              </div>

              {#if trust.kind === 'starting'}
                <div class="flex items-center gap-3 rounded-box bg-base-200 p-4">
                  <span class="loading loading-spinner loading-sm text-primary"></span>
                  <p class="text-sm font-medium">Asking the phone to pair…</p>
                </div>
              {:else if trust.kind === 'pending'}
                <div class="flex items-center gap-3 rounded-box bg-base-200 p-4">
                  <span class="loading loading-spinner loading-sm text-primary"></span>
                  <p class="text-sm font-medium">Waiting for you to tap Trust on the phone…</p>
                </div>
              {:else if trust.kind === 'locked'}
                <div role="alert" class="alert alert-warning alert-soft">
                  <Icon name="lock" size={18} />
                  <div>
                    <p class="font-medium">Unlock the iPhone</p>
                    <p class="mt-1 text-sm opacity-80">The Trust prompt appears right after you unlock it — this screen advances automatically.</p>
                  </div>
                </div>
              {:else if trust.kind === 'denied'}
                <div role="alert" class="alert alert-warning alert-soft">
                  <Icon name="alert" size={18} />
                  <div>
                    <p class="font-medium">“Don't Trust” was tapped on the phone</p>
                    <p class="mt-1 text-sm opacity-80">
                      iOS won't ask again on this connection. <span class="font-medium">Unplug the USB
                      cable and plug it back in</span> — the Trust prompt reappears and pairing
                      continues here automatically.
                    </p>
                  </div>
                </div>
                <div>
                  <button type="button" class="btn btn-ghost btn-sm" onclick={() => selectedUdid && startTrust(selectedUdid)}>
                    <Icon name="refresh" size={14} /> Ask anyway
                  </button>
                </div>
              {:else if trust.kind === 'error'}
                <div role="alert" class="alert alert-error alert-soft">
                  <Icon name="alert" size={18} />
                  <p class="font-medium">{trustError}</p>
                </div>
                <div>
                  <button type="button" class="btn btn-primary" onclick={() => selectedUdid && startTrust(selectedUdid)}>
                    <Icon name="refresh" size={15} />
                    {wifiAuthorizationFailed ? 'Try Wi-Fi setup again' : 'Retry'}
                  </button>
                </div>
              {/if}
            </div>

          {:else}
            <div class="flex flex-col items-center gap-4 py-4 text-center">
              <span class="flex h-14 w-14 items-center justify-center rounded-box bg-success/10 text-success">
                <Icon name="check" size={30} stroke={2.2} />
              </span>
              <div>
                <h4 class="text-lg font-semibold">Paired!</h4>
                <p class="mx-auto mt-1 max-w-md text-sm text-base-content/60">
                  <span class="font-medium text-base-content">{selectedName}</span> is connected to
                  AirVault — you can unplug the cable
                </p>
              </div>
              <div class="mt-1 flex flex-wrap justify-center gap-2">
                <button type="button" class="btn btn-primary" onclick={() => openDevicePage(selectedUdid!)}>
                  <Icon name="phone" size={15} /> Open device page
                </button>
                <button type="button" class="btn btn-ghost" onclick={() => dialog.close()}>Close</button>
              </div>
            </div>
          {/if}
        </div>

        {#if step === 2}
          <div class="flex items-center justify-between border-t border-base-300 pt-3">
            <button type="button" class="btn btn-ghost btn-sm" onclick={() => (step -= 1)}>Back</button>
            <button type="button" class="btn btn-ghost btn-sm" onclick={() => dialog.close()}>Cancel</button>
          </div>
        {/if}
      {/if}
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
