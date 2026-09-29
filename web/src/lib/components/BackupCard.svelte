<script lang="ts">
  import { startBackup, startVerify } from '../api/backups';
  import { errMsg } from '../api/client';
  import type { Device } from '../api/devices';
  import { cancelRun } from '../api/runs';
  import { liveRun } from '../events.svelte';
  import { errorText, type ErrorTextKey } from '../error-text';
  import { restoreSourcesStore } from '../stores.svelte';
  import { formatBytes, formatDateTime, formatSpeed, relativeTime } from '../format';
  import { now } from '../clock';
  import { autoBackupStatus, blockedReason, lastBackupFailure, stageUi } from '../device-ui';
  import AutoBackupModal from './AutoBackupModal.svelte';
  import BackupPasswordModal, { type PasswordMode } from './BackupPasswordModal.svelte';
  import ConfirmDialog from './ConfirmDialog.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Pill from './Pill.svelte';

  let {
    device,
    onrestore,
  }: {
    device: Device;
    /** Open the restore flow with the default snapshot. */
    onrestore: () => void;
  } = $props();

  const udid = $derived(device.udid);
  const reachable = $derived(device.connection !== 'offline');
  const live = $derived(liveRun(udid));
  const isRestore = $derived(live?.restore ?? false);
  const isVerify = $derived(live?.verify ?? false);
  const isCancelling = $derived(live?.cancelling ?? false);
  const isRunning = $derived(live !== null);
  const speed = $derived(formatSpeed(live?.speed));
  const lastFailure = $derived(lastBackupFailure(device, live));
  // ChangePassword and restores talk to the same backupd that is busy during a transfer.
  const blocked = $derived(blockedReason(device, live));

  // The run whose start request is in flight.
  let starting = $state<'backup' | 'verify' | null>(null);
  let cancelRequestedFor = $state<string | null>(null);
  // The confirm dialog belongs to one run: it closes the moment that run ends
  // or starts cancelling.
  let cancelAskedFor = $state<string | null>(null);
  const cancelOpen = $derived(live !== null && cancelAskedFor === live.runId && !isCancelling);
  const cancelPending = $derived(isCancelling || cancelRequestedFor === live?.runId);
  let actionError = $state<string | null>(null);
  // The server keeps each kind's last terminal error until its next run
  // replaces or clears it; a backup's shows as the "failed" pill instead.
  const runError = $derived.by(() => {
    const errors = device.lastRunErrors;
    if (isRunning || !errors) return null;
    if (errors.restore) return errorText(errors.restore, 'restore_failed');
    return errors.verify ? errorText(errors.verify, 'verify_failed') : null;
  });

  async function start(kind: 'backup' | 'verify', failureCode: ErrorTextKey) {
    starting = kind;
    actionError = null;
    try {
      await (kind === 'backup' ? startBackup(udid) : startVerify(udid));
    } catch (error) {
      actionError = errMsg(error, failureCode);
    } finally {
      starting = null;
    }
  }

  async function cancelCurrentRun(runId: string) {
    cancelRequestedFor = runId;
    try {
      await cancelRun(runId);
    } catch (error) {
      // A terminal/cancelling SSE can legitimately beat the HTTP response.
      if (isCancelling || live?.runId !== runId) return;
      cancelRequestedFor = null;
      throw error;
    }
  }

  let passwordMode = $state<PasswordMode | null>(null);
  let autoBackupOpen = $state(false);
  const autoStatus = $derived(autoBackupStatus(device.autoBackup, $now));

  $effect(() => restoreSourcesStore.start());
  const sources = $derived(restoreSourcesStore.data ?? []);
  const sourcesLoaded = $derived(restoreSourcesStore.ready);

  const stored = $derived(sources.find((source) => source.udid === udid) ?? null);
  const hasRestoreSource = $derived(sources.some((source) => !source.damage));
  // This phone's restore points, for the integrity summary.
  const ownPoints = $derived(sources.filter((source) => source.udid === udid));
  const damagedCount = $derived(ownPoints.filter((point) => point.damage).length);
  const damageNote = $derived(
    damagedCount === 0
      ? null
      : damagedCount === ownPoints.length
        ? 'No restore point is intact, so the next backup will be a full one; delete the damaged ones to free their space'
        : "Damaged restore points can't be restored or downloaded; delete them or check again",
  );
  // ISO times in UTC sort as they happen.
  const lastVerified = $derived(
    ownPoints
      .map((point) => point.verifiedAt)
      .filter((at): at is string => at !== undefined)
      .sort()
      .at(-1),
  );
</script>

<div class="card bg-base-100 shadow-sm">
  <div class="card-body gap-5 p-5">
    <div class="flex flex-wrap items-center justify-between gap-4">
      <div class="min-w-0">
        <h2 class="flex items-center gap-2 text-sm font-semibold">
          Backup
          {#if damagedCount > 0}
            <Pill tone="red" dot>{damagedCount} damaged</Pill>
          {:else if lastVerified}
            <span class="tooltip" data-tip={`Integrity checked ${relativeTime(lastVerified, $now)}`}>
              <Pill tone="green" dot>Verified</Pill>
            </span>
          {/if}
        </h2>
        {#if isRunning}
          <p class="mt-0.5 text-xs text-base-content/60">
            {isRestore
              ? 'A restore is running right now'
              : isVerify
                ? 'An integrity check is running right now'
                : 'A backup is running right now'}
          </p>
        {:else if stored}
          <p class="mt-0.5 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs text-base-content/60">
            <span title={formatDateTime(stored.created)} class="text-base-content/80">
              Backed up {relativeTime(stored.created, $now)}
            </span>
            {#if stored.sizeBytes}<span>· {formatBytes(stored.sizeBytes)}</span>{/if}
            {#if stored.encrypted}<span>· encrypted</span>{/if}
            {#if stored.iosVersion}<span>· iOS {stored.iosVersion}</span>{/if}
            {#if lastFailure}
              <span class="tooltip tooltip-error" data-tip={lastFailure}>
                <Pill tone="red" dot>Last run failed</Pill>
              </span>
            {/if}
          </p>
          {#if damageNote}
            <p class="mt-1 text-xs text-error">{damageNote}</p>
          {/if}
        {:else if !sourcesLoaded}
          <p class="mt-0.5 text-xs text-base-content/40">Checking the stored snapshot…</p>
        {:else}
          <p class="mt-0.5 flex items-center gap-2 text-xs text-base-content/60">
            <span>Never backed up</span>
            {#if lastFailure}
              <span class="tooltip tooltip-error" data-tip={lastFailure}>
                <Pill tone="red" dot>Failed</Pill>
              </span>
            {/if}
          </p>
        {/if}
        {#if !reachable && !isRunning && device.paired}
          <p class="mt-1 flex items-center gap-1.5 text-xs text-base-content/50">
            <Icon name="offline" size={12} />
            Offline — a backup started now waits for the phone to come back
          </p>
        {/if}
      </div>
      {#if cancelPending}
        <button type="button" class="btn btn-ghost btn-sm" disabled>
          <span class="loading loading-spinner loading-xs"></span>
          Cancelling…
        </button>
      {:else if live}
        <button
          type="button"
          class="btn btn-ghost btn-sm text-error"
          onclick={() => (cancelAskedFor = live.runId)}
          title={isRestore
            ? 'Stop this restore; the phone then reports it failed and restarts'
            : isVerify
              ? 'Stop the integrity check'
              : 'Stop this attempt and discard the data received during it'}
        >
          <Icon name="x" size={15} />
          Cancel…
        </button>
      {:else}
        <div class="flex flex-wrap gap-2">
          {#if ownPoints.length > 0}
            <button
              type="button"
              class="btn btn-ghost btn-sm"
              disabled={starting !== null}
              onclick={() => start('verify', 'verify_start_failed')}
              title="Read every stored file and mark the restore points whose data no longer reads right"
            >
              {#if starting === 'verify'}
                <span class="loading loading-spinner loading-xs"></span>
              {:else}
                <Icon name="shield" size={14} />
              {/if}
              Verify
            </button>
          {/if}
          {#if hasRestoreSource}
            <button
              type="button"
              class="btn btn-outline btn-sm"
              disabled={starting !== null || !!blocked}
              onclick={onrestore}
              title={blocked ?? 'Put a stored backup onto this phone'}
            >
              <Icon name="backup" size={14} /> Restore…
            </button>
          {/if}
          <button
            type="button"
            class="btn btn-primary btn-sm"
            disabled={starting !== null || !device.paired}
            onclick={() => start('backup', 'backup_start_failed')}
            title={!device.paired
              ? 'Pair the phone over USB first'
              : !reachable
                ? 'The phone is offline — the backup starts as soon as it comes back'
                : 'Update the stored backup (incremental)'}
          >
            {#if starting === 'backup'}
              <span class="loading loading-spinner loading-xs"></span> Starting…
            {:else}
              <Icon name="backup" size={15} /> Back up now
            {/if}
          </button>
        </div>
      {/if}
    </div>

    {#if live}
      {@const percent = live.progress}
      <div class="flex flex-col gap-2 rounded-box bg-base-200 p-3">
        <div>
          <div class="mb-1 flex items-center justify-between gap-2 text-xs">
            <span class="truncate text-base-content/60">{stageUi(live.stage, isRestore)}</span>
            {#if percent > 0}
              <span class="shrink-0 font-mono tabular-nums">{percent}%</span>
            {/if}
          </div>
          {#if percent > 0}
            <progress class="progress progress-primary" value={percent} max="100"></progress>
          {:else}
            <progress class="progress progress-primary"></progress>
          {/if}
        </div>
        {#if live.transferred > 0 || speed}
          <div class="flex items-center justify-between gap-2 font-mono text-xs tabular-nums text-base-content/60">
            <span class="truncate">
              {#if live.transferred > 0}
                {formatBytes(live.transferred)} {isVerify ? 'checked' : 'transferred'}
              {/if}
            </span>
            {#if speed}<span class="shrink-0">{speed}</span>{/if}
          </div>
        {/if}
        {#if live.stage === 'waiting_for_device' && !isRestore}
          <p class="flex items-center gap-1.5 text-xs text-base-content/60">
            <Icon name="info" size={13} />
            Wake the phone or connect it by USB — the backup starts as soon as it appears.
          </p>
        {:else if live.stage === 'preparing' && !isRestore}
          <p class="flex items-center gap-1.5 text-xs text-base-content/60">
            <Icon name="info" size={13} />
            {live.auto
              ? 'Automatic backup — enter the iPhone passcode on the phone within a minute'
              : 'Enter the iPhone passcode on the phone to continue'}
          </p>
        {/if}
      </div>
    {/if}

    <ErrorLine error={actionError ?? runError} size="xs" />

    <div class="flex flex-col gap-3 border-t border-base-300 pt-4">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="min-w-0">
          <p class="flex items-center gap-2 text-sm font-semibold">
            Encryption
            {#if device.encrypted}<Pill tone="green" dot>On</Pill>{:else}<Pill tone="slate" dot>Off</Pill>{/if}
          </p>
          <p class="mt-0.5 text-xs text-base-content/60">
            {device.encrypted
              ? 'Backups are protected with a password set on the phone'
              : 'Health and Keychain data are only included in encrypted backups'}
          </p>
        </div>
        <div class="flex flex-wrap gap-2">
          {#if device.encrypted}
            <button
              type="button"
              class="btn btn-ghost btn-sm"
              disabled={!!blocked}
              title={blocked}
              onclick={() => (passwordMode = 'change')}
            >
              Change password…
            </button>
            <button
              type="button"
              class="btn btn-ghost btn-sm text-error"
              disabled={!!blocked}
              title={blocked}
              onclick={() => (passwordMode = 'disable')}
            >
              Turn off…
            </button>
          {:else}
            <button
              type="button"
              class="btn btn-outline btn-sm"
              disabled={!!blocked}
              title={blocked}
              onclick={() => (passwordMode = 'enable')}
            >
              <Icon name="lock" size={14} /> Set password…
            </button>
          {/if}
        </div>
      </div>
      {#if !reachable}
        <p class="text-xs text-base-content/50">Connect the device (Wi-Fi or USB) to manage encryption.</p>
      {/if}
    </div>

    <div class="flex flex-wrap items-center justify-between gap-3 border-t border-base-300 pt-4">
      <div class="min-w-0">
        <p class="flex items-center gap-2 text-sm font-semibold">
          Automatic backup
          {#if autoStatus}<Pill tone="green" dot>On</Pill>{:else}<Pill tone="slate" dot>Off</Pill>{/if}
        </p>
        {#if autoStatus}
          <p class="mt-0.5 text-xs text-base-content/60">
            <span class="text-base-content/80">{autoStatus.schedule}</span> ·
            <span title={autoStatus.at && formatDateTime(autoStatus.at)}>{autoStatus.next}</span>
          </p>
        {:else}
          <p class="mt-0.5 text-xs text-base-content/60">
            Backs up shortly after the iPhone is unlocked at home; iOS asks for the passcode each time
          </p>
        {/if}
      </div>
      <button
        type="button"
        class="btn btn-ghost btn-sm"
        disabled={!device.paired}
        title={device.paired ? undefined : 'Pair the phone over USB first'}
        onclick={() => (autoBackupOpen = true)}
      >
        <Icon name="clock" size={14} />
        {autoStatus ? 'Change…' : 'Set up…'}
      </button>
    </div>
  </div>
</div>

{#if cancelOpen && live}
  {@const runId = live.runId}
  <ConfirmDialog
    title={`Cancel this ${isRestore ? 'restore' : isVerify ? 'integrity check' : 'backup'}?`}
    icon="x"
    confirmLabel={isRestore ? 'Stop restore' : isVerify ? 'Stop check' : 'Discard attempt'}
    busyLabel="Cancelling…"
    cancelLabel="Keep running"
    failureCode="backup_cancel_failed"
    onconfirm={() => cancelCurrentRun(runId)}
    onclose={() => (cancelAskedFor = null)}
  >
    <p class="py-3 text-sm text-base-content/70">
      {#if isRestore}
        The phone stays on “Restore in Progress” until iOS gives up, then reports that the restore failed and
        restarts. The stored backup is not changed.
      {:else if isVerify}
        The check will stop. What it has already found stays marked.
      {:else}
        This unfinished attempt and all data received during it will be discarded. Existing completed restore points will not be changed.
      {/if}
    </p>
  </ConfirmDialog>
{/if}

<!-- Mounted fresh per open: per-open form state resets by remount. -->
{#if passwordMode !== null}
  <BackupPasswordModal udid={device.udid} name={device.name} mode={passwordMode} onclose={() => (passwordMode = null)} />
{/if}
{#if autoBackupOpen}
  <AutoBackupModal
    udid={device.udid}
    name={device.name}
    current={device.autoBackup}
    onclose={() => (autoBackupOpen = false)}
  />
{/if}
