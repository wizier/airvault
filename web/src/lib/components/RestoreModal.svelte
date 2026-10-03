<script lang="ts">
  // The card is the run's surface (progress via SSE, failures via the server's
  // last-restore-error); only errors of the start request itself show inline,
  // where they can be corrected.
  import { startRestore, type RestorePoint } from '../api/backups';
  import { formatBytes, formatDateTime, relativeTime, shortUdid } from '../format';
  import { hardwareResources, restoreSourcesStore } from '../stores.svelte';
  import { Submit } from '../submit.svelte';
  import Alert from './Alert.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import FindMyAlert from './FindMyAlert.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import PasswordField from './PasswordField.svelte';

  let {
    udid,
    name,
    iosVersion,
    activationState,
    preselect = null,
    onclose,
  }: {
    udid: string;
    name: string;
    /** The phone's current iOS version; a backup from a newer iOS is refused. */
    iosVersion?: string;
    /** Lockdown ActivationState; "Unactivated" shows the activation notice. */
    activationState?: string;
    /** Snapshot to preselect on open instead of the default (own newest). */
    preselect?: string | null;
    onclose: () => void;
  } = $props();

  let modal: Modal;
  // Every on-disk backup that could be applied (own + other phones'), newest
  // first; a damaged one never can.
  $effect(() => restoreSourcesStore.start());
  const sources = $derived((restoreSourcesStore.data ?? []).filter((source) => !source.damage));
  // Preselected snapshot or this phone's newest; never auto-pick a foreign phone.
  // Mount-time snapshot — `sources` refreshes while open must not clobber picks.
  // svelte-ignore state_referenced_locally
  const initialPoint =
    (preselect && sources.find((s) => s.snapshotId === preselect)) || sources.find((s) => s.udid === udid);
  let selectedUdid = $state(initialPoint?.udid ?? '');
  let selected = $state(initialPoint?.snapshotId ?? '');
  let password = $state('');
  // Standard restore follows Finder's effective behavior. Advanced controls
  // are phrased as exceptions, so every enabled switch is deliberate.
  let overrides = $state({
    keepCurrentSettings: false,
    skipSystemFiles: false,
    keepItemsNotInBackup: false,
    doNotRestart: false,
  });
  const OVERRIDES: { key: keyof typeof overrides; label: string; hint: string }[] = [
    { key: 'keepCurrentSettings', label: 'Keep current settings', hint: "Keep the phone's settings, not the backup's" },
    { key: 'skipSystemFiles', label: 'Skip system files', hint: 'Do not restore system-level files stored in the backup' },
    { key: 'keepItemsNotInBackup', label: 'Keep items not in the backup', hint: 'Merge backup data with existing phone content' },
    { key: 'doNotRestart', label: 'Do not restart', hint: 'Restart the phone manually after the restore' },
  ];
  const submit = new Submit('restore_start_failed');

  // The source phones present on disk (one entry each), this phone first.
  const phones = $derived.by(() => {
    const byUdid = new Map<string, string>();
    for (const s of sources) {
      if (!byUdid.has(s.udid)) byUdid.set(s.udid, s.udid === udid ? 'This phone' : s.deviceName || shortUdid(s.udid));
    }
    return [...byUdid]
      .map(([id, name]) => ({ udid: id, name }))
      .sort((a, b) => (a.udid === udid ? -1 : b.udid === udid ? 1 : a.name.localeCompare(b.name)));
  });
  const points = $derived(sources.filter((s) => s.udid === selectedUdid));
  const sel = $derived(points.find((s) => s.snapshotId === selected) ?? null);
  const crossDevice = $derived(sel !== null && sel.udid !== udid);
  const wrongPassword = $derived(submit.code === 'invalid_backup_password');
  const overrideCount = $derived(Object.values(overrides).filter(Boolean).length);

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
    if (submit.busy || !sel) return;
    const options = {
      snapshotId: sel.snapshotId,
      password: sel.encrypted ? password : '',
      systemFiles: !overrides.skipSystemFiles,
      reboot: !overrides.doNotRestart,
      settingsFromBackup: !overrides.keepCurrentSettings,
      removeItemsNotRestored: !overrides.keepItemsNotInBackup,
    };
    if (!(await submit.run(() => startRestore(udid, options)))) return;
    password = '';
    modal.close();
  }
</script>

<Modal
  bind:this={modal}
  title={`Restore ${name}?`}
  locked={submit.busy}
  class="flex max-h-[85vh] max-w-2xl flex-col overflow-hidden"
  {onclose}
>
  <!-- Content-height card would jump as warnings/options appear; a scrollable
       middle keeps the box stable like the Files/Apps modals. -->
  <div class="-mx-1 min-h-0 flex-1 overflow-auto px-1">
    <div class="mt-3 flex flex-col gap-3">
      <label class="flex flex-col gap-1.5">
        <span class="label">Restore from</span>
        <select
          class="select w-full"
          bind:value={selectedUdid}
          disabled={submit.busy}
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
          disabled={submit.busy || selectedUdid === ''}
        >
          <option value="" disabled>{selectedUdid === '' ? 'Choose a phone first' : 'Select a backup…'}</option>
          {#each points as s (s.snapshotId)}
            <option value={s.snapshotId}>{backupLabel(s)}</option>
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
        disabled={submit.busy}
        error={wrongPassword ? 'That password does not unlock the selected backup. Try again.' : null}
        focus={wrongPassword}
      />
    {/if}

    {#if findMyOn}
      <FindMyAlert {udid} />
    {/if}

    {#if crossDevice}
      <Alert tone="warning" class="mt-3 text-sm">
        <span class="font-medium">{name}</span> receives the backup of
        <span class="font-medium">{sel?.deviceName || 'another phone'}</span>
      </Alert>
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
      <Alert tone="warning" class="mt-3 text-sm">
        This phone isn't activated yet — it will be activated as the first step
      </Alert>
    {/if}

    {#if versionBlocked}
      <Alert tone="error" class="mt-3 text-sm">
        This backup was made on iOS {sel?.iosVersion}, newer than the phone's iOS
        {iosVersion} — update the phone first.
      </Alert>
    {/if}

    {#if spaceShort && sel}
      <Alert tone="error" class="mt-3 text-sm">
        The backup ({formatBytes(sel.sizeBytes)}) is larger than the phone's total storage
        ({formatBytes(capacityBytes)}) — the restore will not fit on this phone.
      </Alert>
    {/if}

    <details class="collapse collapse-arrow mt-3 border border-base-300 bg-base-100">
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
        {#each OVERRIDES as o (o.key)}
          <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
            <input type="checkbox" class="checkbox checkbox-sm" bind:checked={overrides[o.key]} disabled={submit.busy} />
            <span class="text-sm leading-snug">
              <span>{o.label}</span>
              <span class="mt-0.5 block text-xs text-base-content/60">{o.hint}</span>
            </span>
          </label>
        {/each}
      </div>
    </details>

    <p class="mt-3 flex items-center gap-1.5 text-xs text-base-content/60">
      <Icon name="info" size={13} />
      Apps from the backup re-download from the App Store
    </p>

    {#if !wrongPassword}
      <ErrorLine error={submit.failure} className="mt-3" />
    {/if}
  </div>

  {#snippet actions()}
    <button type="button" class="btn btn-ghost" disabled={submit.busy} onclick={() => modal.close()}>Cancel</button>
    <button
      type="button"
      class="btn btn-error"
      disabled={submit.busy || !sel || (sel.encrypted && !password) || versionBlocked || findMyOn}
      onclick={confirm}
    >
      {#if submit.busy}
        <span class="loading loading-spinner loading-xs"></span>
        Starting…
      {:else if submit.failure}
        <Icon name="restore" size={15} /> Try restore again
      {:else}
        <Icon name="restore" size={15} />
        Restore
      {/if}
    </button>
  {/snippet}
</Modal>
