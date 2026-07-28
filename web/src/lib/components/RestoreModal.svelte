<script lang="ts">
  // Restore confirmation: collects the options and launches the run. The card
  // is the run's surface — progress via SSE, failures via the server's
  // last-restore-error; only errors of the start request itself show inline,
  // where they can be corrected.
  import { startRestore, type RestorePoint } from '../api/backups';
  import { errRef } from '../api/client';
  import type { ErrorRef } from '../error-text';
  import { formatBytes, formatDateTime, relativeTime, shortUdid } from '../format';
  import { modalOpen } from '../modal';
  import { hardwareResources } from '../stores.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import PasswordField from './PasswordField.svelte';

  let {
    udid,
    name,
    iosVersion,
    activationState,
    preselect = null,
    sources,
    open = $bindable(false),
  }: {
    udid: string;
    name: string;
    /** The phone's current iOS version; a backup from a newer iOS is refused. */
    iosVersion?: string;
    /** Lockdown ActivationState; "Unactivated" shows the activation notice. */
    activationState?: string;
    /** Snapshot to preselect on open instead of the default (own newest). */
    preselect?: string | null;
    /** Every on-disk backup that could be applied (own + other phones'). */
    sources: RestorePoint[];
    open?: boolean;
  } = $props();

  const sourceKey = (s: RestorePoint) => s.snapshotId;
  // Preselected snapshot or this phone's newest; never auto-pick a foreign phone.
  // Mount-time snapshot — `sources` refreshes while open must not clobber picks.
  // svelte-ignore state_referenced_locally
  const initialPoint =
    (preselect ? sources.find((s) => sourceKey(s) === preselect) : null) ??
    sources
      .filter((s) => s.udid === udid)
      .sort((a, b) => b.created.localeCompare(a.created))[0] ??
    null;
  let selectedUdid = $state(initialPoint ? initialPoint.udid ?? '' : '');
  let selected = $state(initialPoint ? sourceKey(initialPoint) : '');
  let password = $state('');
  // Standard restore follows Finder's effective behavior. Advanced controls
  // are phrased as exceptions, so every enabled switch is deliberate.
  let advancedOpen = $state(false);
  let keepCurrentSettings = $state(false);
  let skipSystemFiles = $state(false);
  let keepItemsNotInBackup = $state(false);
  let doNotRestart = $state(false);
  let busy = $state(false);
  let failure = $state<ErrorRef | null>(null);

  // The source phones present on disk (one entry each), this phone first.
  const phones = $derived.by(() => {
    const byUdid = new Map<string, string>();
    for (const s of sources) {
      const key = s.udid ?? '';
      if (!byUdid.has(key)) byUdid.set(key, key === udid ? 'This phone' : s.deviceName || shortUdid(key));
    }
    return [...byUdid]
      .map(([id, name]) => ({ udid: id, name }))
      .sort((a, b) => (a.udid === udid ? -1 : b.udid === udid ? 1 : a.name.localeCompare(b.name)));
  });
  const points = $derived(
    sources
      .filter((s) => (s.udid ?? '') === selectedUdid)
      .sort((a, b) => b.created.localeCompare(a.created)),
  );
  const sel = $derived(points.find((s) => sourceKey(s) === selected) ?? null);
  const crossDevice = $derived(sel !== null && sel.udid !== udid);
  const wrongPassword = $derived(failure?.code === 'invalid_backup_password');
  const overrideCount = $derived(
    Number(keepCurrentSettings) +
      Number(skipSystemFiles) +
      Number(keepItemsNotInBackup) +
      Number(doNotRestart),
  );

  // Instant preflights: hardware gives Find My and storage capacity, the
  // catalog gives iOS versions. Unknown values never block — the server
  // re-enforces.
  const hwRes = $derived(hardwareResources.for(udid));
  const findMyOn = $derived(hwRes.data?.findMyEnabled === true);
  const capacityBytes = $derived(hwRes.data?.diskDataCapacity);
  // Advisory only — snapshot size counts duplicates, so it is an upper bound;
  // iOS applies the authoritative per-domain space check during the restore.
  const spaceShort = $derived(
    crossDevice && sel !== null && capacityBytes !== undefined && sel.sizeBytes > capacityBytes,
  );
  const versionBlocked = $derived(sel !== null && newerIOS(sel.iosVersion, iosVersion));

  // Mirrors the server rule: a backup from a strictly newer iOS is refused.
  function newerIOS(a?: string, b?: string): boolean {
    if (!a || !b) return false;
    const av = a.split('.').map(Number);
    const bv = b.split('.').map(Number);
    for (let i = 0; i < Math.max(av.length, bv.length); i++) {
      const x = av[i] || 0;
      const y = bv[i] || 0;
      if (x !== y) return x > y;
    }
    return false;
  }

  function backupLabel(s: RestorePoint): string {
    const parts = [
      `${formatDateTime(s.created)} (${relativeTime(s.created)})`,
      s.sizeBytes ? formatBytes(s.sizeBytes) : '',
      s.iosVersion ? `iOS ${s.iosVersion}` : '',
      s.encrypted ? 'encrypted' : '',
    ].filter(Boolean);
    return parts.join(' · ');
  }

  // A fresh Find My/storage verdict per open; the subscription lives while mounted.
  $effect(() => {
    void hwRes.refresh();
    return hwRes.start();
  });

  async function confirm() {
    if (busy || !sel) return;
    busy = true;
    failure = null;
    try {
      await startRestore(udid, {
        snapshotId: sel.snapshotId,
        password: sel.encrypted ? password : '',
        systemFiles: !skipSystemFiles,
        reboot: !doNotRestart,
        settingsFromBackup: !keepCurrentSettings,
        removeItemsNotRestored: !keepItemsNotInBackup,
      });
      password = '';
      open = false;
    } catch (err) {
      failure = errRef(err, 'restore_start_failed');
    } finally {
      busy = false;
    }
  }
</script>

<dialog
  class="modal"
  {@attach modalOpen(open)}
  oncancel={(event) => busy && event.preventDefault()}
  onclose={() => (open = false)}
>
  <div class="modal-box flex max-h-[85vh] max-w-2xl flex-col overflow-hidden">
    <h3 class="shrink-0 text-lg font-bold">Restore {name}?</h3>

    <!-- Content-height card would jump as warnings/options appear; a scrollable
         middle keeps the box stable like the Files/Apps modals. -->
    <div class="-mx-1 min-h-0 flex-1 overflow-auto px-1">
    <div class="mt-3 flex flex-col gap-3">
      <label class="flex flex-col gap-1.5">
        <span class="label">Restore from</span>
        <select
          class="select w-full"
          bind:value={selectedUdid}
          disabled={busy}
          onchange={() => (selected = '')}
        >
          <option value="" disabled>Select a phone…</option>
          {#each phones as p (p.udid)}
            <option value={p.udid}>{p.name}</option>
          {/each}
        </select>
      </label>
      <label class="flex flex-col gap-1.5">
        <span class="label">Backup</span>
        <select
          class="select w-full"
          bind:value={selected}
          disabled={busy || selectedUdid === ''}
        >
          <option value="" disabled>{selectedUdid === '' ? 'Choose a phone first' : 'Select a backup…'}</option>
          {#each points as s (sourceKey(s))}
            <option value={sourceKey(s)}>{backupLabel(s)}</option>
          {/each}
        </select>
      </label>
    </div>

    {#if sel?.encrypted}
      <PasswordField
        className="mt-3"
        label="Backup password"
        bind:value={password}
        placeholder="Required — the backup is encrypted"
        disabled={busy}
        error={wrongPassword ? 'That password does not unlock the selected backup. Try again.' : null}
        focus={wrongPassword}
      />
    {/if}

    {#if findMyOn}
      <div role="alert" class="alert alert-warning alert-soft mt-3">
        <Icon name="alert" size={18} />
        <div class="text-sm">
          <p class="font-medium">Find My iPhone is on</p>
          <p class="mt-1 opacity-80">
            The phone refuses restore while Find My is on.
            Turn it off (Settings → your name → Find My), then check again.
          </p>
        </div>
        <button
          type="button"
          class="btn btn-ghost btn-xs"
          disabled={hwRes.loading}
          onclick={() => hwRes.refresh()}
        >
          {#if hwRes.loading}<span class="loading loading-spinner loading-xs"></span>{/if}
          Check again
        </button>
      </div>
    {/if}

    {#if crossDevice}
      <div role="alert" class="alert alert-warning alert-soft mt-3">
        <Icon name="alert" size={18} />
        <span class="text-sm">
          <span class="font-medium">{name}</span> receives the backup of
          <span class="font-medium">{sel?.deviceName || 'another phone'}</span>
        </span>
      </div>
      <div class="mt-2 rounded-box bg-base-200 p-3 text-xs text-base-content/70">
        <p class="font-medium text-base-content/80">Moving to this phone</p>
        <p class="mt-1">
          A new phone can be paired right from the setup assistant — on the
          “Apps &amp; Data” screen, choose “Restore from Mac or PC” and connect
          the cable
        </p>
      </div>
    {/if}

    {#if activationState === 'Unactivated'}
      <div role="alert" class="alert alert-warning alert-soft mt-3">
        <Icon name="alert" size={18} />
        <p class="text-sm">
          This phone isn't activated yet — it will be activated as the first step
        </p>
      </div>
    {/if}

    {#if versionBlocked}
      <div role="alert" class="alert alert-error alert-soft mt-3">
        <Icon name="alert" size={18} />
        <p class="text-sm">
          This backup was made on iOS {sel?.iosVersion}, newer than the phone's iOS
          {iosVersion} — update the phone first.
        </p>
      </div>
    {/if}

    {#if spaceShort && sel}
      <div role="alert" class="alert alert-error alert-soft mt-3">
        <Icon name="alert" size={18} />
        <p class="text-sm">
          The backup ({formatBytes(sel.sizeBytes)}) is larger than the phone's total storage
          ({formatBytes(capacityBytes)}) — the restore will not fit on this phone.
        </p>
      </div>
    {/if}

    <details bind:open={advancedOpen} class="collapse collapse-arrow mt-3 border border-base-300 bg-base-100">
      <summary class="collapse-title flex items-center justify-between gap-3 pr-10 font-medium">
        <span>Advanced restore options</span>
        <span class="text-sm font-normal {overrideCount === 0 ? 'text-base-content/60' : 'text-warning'}">
          {overrideCount === 0 ? 'Default' : `${overrideCount} ${overrideCount === 1 ? 'change' : 'changes'}`}
        </span>
      </summary>
      <div class="collapse-content flex flex-col gap-2">
        <p class="text-xs text-base-content/60">
          Default restores settings and system files, removes items outside the backup, and restarts the phone
        </p>
        <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
          <input type="checkbox" class="checkbox checkbox-sm" bind:checked={keepCurrentSettings} disabled={busy} />
          <span class="text-sm leading-snug">
            <span>Keep current settings</span>
            <span class="mt-0.5 block text-xs text-base-content/60">
              Keep the phone's settings, not the backup's
            </span>
          </span>
        </label>
        <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
          <input type="checkbox" class="checkbox checkbox-sm" bind:checked={skipSystemFiles} disabled={busy} />
          <span class="text-sm leading-snug">
            <span>Skip system files</span>
            <span class="mt-0.5 block text-xs text-base-content/60">
              Do not restore system-level files stored in the backup
            </span>
          </span>
        </label>
        <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
          <input type="checkbox" class="checkbox checkbox-sm" bind:checked={keepItemsNotInBackup} disabled={busy} />
          <span class="text-sm leading-snug">
            <span>Keep items not in the backup</span>
            <span class="mt-0.5 block text-xs text-base-content/60">
              Merge backup data with existing phone content
            </span>
          </span>
        </label>
        <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
          <input type="checkbox" class="checkbox checkbox-sm" bind:checked={doNotRestart} disabled={busy} />
          <span class="text-sm leading-snug">
            <span>Do not restart</span>
            <span class="mt-0.5 block text-xs text-base-content/60">
              Restart the phone manually after the restore
            </span>
          </span>
        </label>
      </div>
    </details>

    <p class="mt-3 flex items-center gap-1.5 text-xs text-base-content/60">
      <Icon name="info" size={13} />
      Apps from the backup re-download from the App Store
    </p>

    {#if !wrongPassword}
      <ErrorLine {failure} className="mt-3" />
    {/if}
    </div>

    <div class="modal-action shrink-0">
      <button type="button" class="btn btn-ghost" disabled={busy} onclick={() => (open = false)}>Cancel</button>
      <button
        type="button"
        class="btn btn-error"
        disabled={busy || !sel || (sel.encrypted && !password) || versionBlocked || findMyOn}
        onclick={confirm}
      >
        {#if busy}
          <span class="loading loading-spinner loading-xs"></span>
          Starting…
        {:else if failure}
          <Icon name="backup" size={15} /> Try restore again
        {:else}
          <Icon name="backup" size={15} />
          Restore
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close" disabled={busy}>close</button>
  </form>
</dialog>
