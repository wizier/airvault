<script lang="ts">
  // Backup snapshot, live transfer controls, encryption and restore. History is a
  // separate feature card but both react to the same SSE invalidation.
  import { startBackup } from '../api/backups';
  import { errRef } from '../api/client';
  import type { Device } from '../api/devices';
  import { cancelRun } from '../api/runs';
  import { liveRun } from '../events.svelte';
  import { errorRef, errorText, type ErrorRef } from '../error-text';
  import { restoreSourcesStore } from '../stores.svelte';
  import { formatBytes, formatDateTime, formatSpeed, relativeTime } from '../format';
  import { modalOpen } from '../modal';
  import { now } from '../clock';
  import { backupStatus, stageUi } from '../device-ui';
  import BackupPasswordModal, { type PasswordMode } from './BackupPasswordModal.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Pill from './Pill.svelte';
  import RestoreModal from './RestoreModal.svelte';

  let { device }: { device: Device } = $props();

  const udid = $derived(device.udid);
  const reachable = $derived(device.connection !== 'offline');
  const live = $derived(liveRun(udid));
  const isRestore = $derived(live?.restore ?? false);
  const isCancelling = $derived(live?.cancelling ?? false);
  const status = $derived(backupStatus(device, live));
  const isRunning = $derived(status === 'running');
  const isBackupRunning = $derived(isRunning && !isRestore);
  const isRestoreRunning = $derived(isRunning && isRestore);
  const speed = $derived(formatSpeed(live?.speed));
  const lastRunFailed = $derived(status === 'failed');
  const lastFailureText = $derived(
    errorText(device.lastRunErrors?.backup ?? 'backup_failed', 'backup_failed'),
  );

  let busy = $state(false);
  let cancelFailure = $state<ErrorRef | null>(null);
  let cancelRequestedFor = $state<string | null>(null);
  // The confirm dialog closes itself the moment the run ends or starts cancelling.
  let cancelAsked = $state(false);
  const cancelOpen = $derived(cancelAsked && live !== null && !isCancelling);
  const cancelPending = $derived(isCancelling || cancelRequestedFor === live?.runId);
  let actionFailure = $state<ErrorRef | null>(null);
  // The server records each restore's terminal error; it shows in the Restore
  // section until the next restore run replaces or clears it.
  const restoreFailure = $derived(
    !isRunning && device.lastRunErrors?.restore
      ? errorRef(device.lastRunErrors.restore, 'restore_failed')
      : null,
  );

  async function backup() {
    if (busy || isRunning) return;
    busy = true;
    actionFailure = null;
    try {
      await startBackup(udid);
    } catch (error) {
      actionFailure = errRef(error, 'backup_start_failed');
    } finally {
      busy = false;
    }
  }

  function askCancel() {
    if (!live || cancelPending) return;
    cancelFailure = null;
    cancelAsked = true;
  }

  async function confirmCancel() {
    if (!live || cancelPending) return;
    const runID = live.runId;
    cancelRequestedFor = runID;
    cancelFailure = null;
    try {
      await cancelRun(runID);
      cancelAsked = false;
    } catch (error) {
      // A terminal/cancelling SSE can legitimately beat the HTTP response.
      if (!isCancelling && live?.runId === runID) {
        cancelFailure = errRef(error, 'backup_cancel_failed');
        cancelRequestedFor = null;
      }
    }
  }

  let passwordMode = $state<PasswordMode | null>(null);
  // ChangePassword talks to the same backupd that is busy during a transfer.
  const passwordBlocked = $derived(!reachable || isRunning);
  const passwordBlockedTitle = $derived(
    !reachable ? 'Device is offline' : isRunning ? 'A backup or restore is running' : undefined,
  );

  let restoreOpen = $state(false);
  let restorePreselect = $state<string | null>(null);

  /** History-row shortcut: open the restore modal with this snapshot chosen. */
  export function openRestore(snapshotId: string) {
    restorePreselect = snapshotId;
    restoreOpen = true;
  }

  $effect(() => restoreSourcesStore.start());
  const sources = $derived(restoreSourcesStore.data ?? []);
  const sourcesLoaded = $derived(restoreSourcesStore.ready);

  const stored = $derived(sources.find((source) => source.udid === udid) ?? null);
  const hasRestoreSource = $derived(sources.length > 0);
</script>

<div class="card bg-base-100 shadow-sm">
  <div class="card-body gap-5 p-5">
    <!-- Shared run widgets; the running kind decides which section renders them. -->
    {#snippet cancelControl()}
      {#if cancelPending}
        <button type="button" class="btn btn-ghost btn-sm" disabled>
          <span class="loading loading-spinner loading-xs"></span>
          Cancelling…
        </button>
      {:else if live}
        <button
          type="button"
          class="btn btn-ghost btn-sm text-error"
          onclick={askCancel}
          title={isRestore
            ? 'Stop this restore before it completes'
            : 'Stop this attempt and discard the data received during it'}
        >
          <Icon name="x" size={15} />
          Cancel…
        </button>
      {/if}
    {/snippet}

    {#snippet runProgress()}
      {@const percent = live?.progress ?? 0}
      <div class="flex flex-col gap-2 rounded-box bg-base-200 p-3">
        <div>
          <div class="mb-1 flex items-center justify-between gap-2 text-xs">
            <span class="truncate text-base-content/60">{stageUi(live?.stage, isRestore)}</span>
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
        {#if (live?.transferred ?? 0) > 0 || speed}
          <div class="flex items-center justify-between gap-2 font-mono text-xs tabular-nums text-base-content/60">
            <span class="truncate">
              {#if (live?.transferred ?? 0) > 0}{formatBytes(live?.transferred)} transferred{/if}
            </span>
            {#if speed}<span class="shrink-0">{speed}</span>{/if}
          </div>
        {/if}
        {#if live?.stage === 'waiting_for_device' && !isRestore}
          <p class="flex items-center gap-1.5 text-xs text-base-content/60">
            <Icon name="info" size={13} />
            Wake the phone or connect it by USB — the backup starts as soon as it appears.
          </p>
        {:else if live?.stage === 'preparing' && !isRestore}
          <p class="flex items-center gap-1.5 text-xs text-base-content/60">
            <Icon name="info" size={13} />
            Enter the iPhone passcode on the phone to continue
          </p>
        {/if}
      </div>
    {/snippet}

    <div class="flex flex-wrap items-center justify-between gap-4">
      <div class="min-w-0">
        <h2 class="text-sm font-semibold">Backup</h2>
        {#if isBackupRunning}
          <p class="mt-0.5 text-xs text-base-content/60">A backup is running right now</p>
        {:else if stored}
          <p class="mt-0.5 flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs text-base-content/60">
            <span title={formatDateTime(stored.created)} class="text-base-content/80">
              Backed up {relativeTime(stored.created, $now)}
            </span>
            {#if stored.sizeBytes}<span>· {formatBytes(stored.sizeBytes)}</span>{/if}
            {#if stored.encrypted}<span>· encrypted</span>{/if}
            {#if stored.iosVersion}<span>· iOS {stored.iosVersion}</span>{/if}
            {#if lastRunFailed}
              <span class="tooltip tooltip-error" data-tip={lastFailureText}>
                <Pill tone="red" dot>Last run failed</Pill>
              </span>
            {/if}
          </p>
        {:else if !sourcesLoaded}
          <p class="mt-0.5 text-xs text-base-content/40">Checking the stored snapshot…</p>
        {:else}
          <p class="mt-0.5 flex items-center gap-2 text-xs text-base-content/60">
            <span>Never backed up</span>
            {#if lastRunFailed}
              <span class="tooltip tooltip-error" data-tip={lastFailureText}>
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
      {#if isBackupRunning}
        {@render cancelControl()}
      {:else}
        <button
          type="button"
          class="btn btn-primary btn-sm"
          disabled={busy || isRunning || !device.paired}
          onclick={backup}
          title={!device.paired
            ? 'Pair the phone over USB first'
            : isRunning
              ? 'A backup or restore is running'
              : !reachable
                ? 'The phone is offline — the backup starts as soon as it comes back'
                : 'Update the stored backup (incremental)'}
        >
          {#if busy}
            <span class="loading loading-spinner loading-xs"></span> Starting…
          {:else}
            <Icon name="backup" size={15} /> Back up now
          {/if}
        </button>
      {/if}
    </div>

    {#if isBackupRunning}
      {@render runProgress()}
    {/if}

    <ErrorLine failure={actionFailure} size="xs" />

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
              disabled={passwordBlocked}
              title={passwordBlockedTitle}
              onclick={() => (passwordMode = 'change')}
            >
              Change password…
            </button>
            <button
              type="button"
              class="btn btn-ghost btn-sm text-error"
              disabled={passwordBlocked}
              title={passwordBlockedTitle}
              onclick={() => (passwordMode = 'disable')}
            >
              Turn off…
            </button>
          {:else}
            <button
              type="button"
              class="btn btn-outline btn-sm"
              disabled={passwordBlocked}
              title={passwordBlockedTitle}
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

    <div class="flex flex-col gap-3 border-t border-base-300 pt-4">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <div class="min-w-0">
          <p class="text-sm font-semibold">Restore</p>
          <p class="mt-0.5 text-xs text-base-content/60">
            {#if isRestoreRunning}
              A restore is running right now
            {:else if stored}
              Apply data from a stored snapshot to this phone
            {:else if hasRestoreSource}
              No backup of this phone yet — but another phone's backup can be migrated onto it
            {:else if !sourcesLoaded}
              Checking stored backups…
            {:else}
              Nothing to restore yet — run a backup first
            {/if}
          </p>
        </div>
        {#if isRestoreRunning}
          {@render cancelControl()}
        {:else}
          <button
            type="button"
            class="btn btn-outline btn-sm"
            disabled={!reachable || isRunning || !hasRestoreSource}
            onclick={() => {
              restorePreselect = null;
              restoreOpen = true;
            }}
            title={!hasRestoreSource
              ? 'No valid backup on disk'
              : !reachable
                ? 'Device is offline'
                : isRunning
                  ? 'A backup or restore is running'
                  : 'Put a stored snapshot onto the phone'}
          >
            <Icon name="backup" size={14} /> Restore…
          </button>
        {/if}
      </div>
      {#if isRestoreRunning}
        {@render runProgress()}
      {/if}
      <ErrorLine failure={restoreFailure} size="xs" />
    </div>
  </div>
</div>

<dialog
  class="modal"
  {@attach modalOpen(cancelOpen)}
  oncancel={(event) => cancelPending && event.preventDefault()}
  onclose={() => (cancelAsked = false)}
>
  <div class="modal-box">
    <h3 class="text-lg font-bold">Cancel this {isRestore ? 'restore' : 'backup'}?</h3>
    <p class="py-3 text-sm text-base-content/70">
      {#if isRestore}
        The restore will stop before completing. The stored backup itself will not be changed.
      {:else}
        This unfinished attempt and all data received during it will be discarded. Existing completed restore points will not be changed.
      {/if}
    </p>
    <ErrorLine failure={cancelFailure} />
    <div class="modal-action">
      <button type="button" class="btn btn-ghost" disabled={cancelPending} onclick={() => (cancelAsked = false)}>Keep running</button>
      <button
        type="button"
        class="btn btn-error"
        disabled={cancelPending}
        onclick={confirmCancel}
      >
        {#if cancelPending}
          <span class="loading loading-spinner loading-xs"></span>
          Cancelling…
        {:else}
          <Icon name="x" size={15} />
          {isRestore ? 'Stop restore' : 'Discard attempt'}
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close" disabled={cancelPending}>close</button>
  </form>
</dialog>

<!-- Mounted fresh per open: per-open form state resets by remount. -->
{#if passwordMode !== null}
  <BackupPasswordModal udid={device.udid} name={device.name} bind:mode={passwordMode} />
{/if}
{#if restoreOpen}
  <RestoreModal
    udid={device.udid}
    name={device.name}
    iosVersion={device.iosVersion}
    activationState={device.activationState}
    preselect={restorePreselect}
    {sources}
    bind:open={restoreOpen}
  />
{/if}
