<script lang="ts">
  // Failed attempts are runtime events; this list contains only completed
  // restore points that still exist. One dialog serves single and multi
  // deletion; the freed-space estimate covers the whole selection.
  import { untrack } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import { backupDownloadUrl, snapshotsReclaimable, type RestorePoint } from '../api/backups';
  import type { Device } from '../api/devices';
  import { blockedReason } from '../device-ui';
  import { liveRun } from '../events.svelte';
  import { deleteRestorePoints, restorePointResources } from '../stores.svelte';
  import { formatBytes, formatDateTime, formatDuration, relativeTime } from '../format';
  import { now } from '../clock';
  import ConfirmDialog from './ConfirmDialog.svelte';
  import EmptyState from './EmptyState.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';

  let {
    device,
    onrestore,
  }: {
    device: Device;
    /** Open the restore flow with this snapshot preselected. */
    onrestore: (snapshotId: string) => void;
  } = $props();

  const udid = $derived(device.udid);

  const points = $derived(restorePointResources.for(udid));
  $effect(() => points.start());
  const restorePoints = $derived(points.data ?? []);
  const pointsLoaded = $derived(points.ready);
  const pointsError = $derived(points.loadError('backup_history_failed'));

  const live = $derived(liveRun(udid));
  const isRunning = $derived(live !== null);
  const restoreBlocked = $derived(blockedReason(device, live));
  // Checkboxes appear only in selection mode; leaving it drops the selection.
  let selecting = $state(false);
  const selected = new SvelteSet<string>();
  let pending = $state<RestorePoint[]>([]);
  let reclaim = $state<Promise<number>>();

  // The selection acts only on listed points — a refresh may have dropped some.
  const selectedPoints = $derived(restorePoints.filter((point) => selected.has(point.snapshotId)));
  const allSelected = $derived(
    restorePoints.length > 0 && selectedPoints.length === restorePoints.length,
  );

  // Nothing left to select (e.g. everything was deleted) ends the mode.
  $effect(() => {
    if (restorePoints.length === 0) untrack(stopSelecting);
  });

  function stopSelecting() {
    selecting = false;
    selected.clear();
  }

  function toggleAll(select: boolean) {
    if (!select) {
      selected.clear();
      return;
    }
    for (const point of restorePoints) selected.add(point.snapshotId);
  }

  function askDelete(targets: RestorePoint[]) {
    pending = targets;
    // Honest "space freed": data no kept restore point still references.
    reclaim = snapshotsReclaimable(udid, targets.map((point) => point.snapshotId));
  }

  async function confirmDelete() {
    await deleteRestorePoints(udid, pending.map((point) => point.snapshotId));
    stopSelecting();
  }
</script>

<section class="flex flex-col gap-3">
  <div class="flex min-h-6 items-center justify-between gap-2">
    <h2 class="text-xs font-semibold uppercase tracking-wide text-base-content/50">Restore points</h2>
    {#if pointsLoaded && restorePoints.length > 0}
      <div class="flex items-center gap-2">
        {#if selecting}
          <button
            type="button"
            class="btn btn-error btn-xs"
            disabled={isRunning || selectedPoints.length === 0}
            onclick={() => askDelete(selectedPoints)}
            title={isRunning ? 'A backup or restore is running' : 'Delete the selected restore points'}
          >
            <Icon name="trash" size={13} />
            Delete selected ({selectedPoints.length})
          </button>
          <button type="button" class="btn btn-ghost btn-xs" onclick={stopSelecting}>Cancel</button>
        {:else}
          <button type="button" class="btn btn-ghost btn-xs" onclick={() => (selecting = true)}>
            Select
          </button>
        {/if}
      </div>
    {/if}
  </div>

  {#if pointsError}
    <ErrorLine error={pointsError} variant="alert" />
  {:else if !pointsLoaded}
    <div class="flex items-center gap-3 rounded-box bg-base-100 p-4 text-sm text-base-content/60 shadow-sm">
      <span class="loading loading-spinner loading-sm"></span>
      Loading restore points…
    </div>
  {:else if restorePoints.length === 0}
    <EmptyState icon="clock" title="No restore points yet" message="Completed backups will appear here" />
  {:else}
    <div class="card overflow-x-auto bg-base-100 shadow-sm">
      <table class="table table-sm">
        <thead>
          <tr class="text-xs">
            {#if selecting}
              <th class="w-0">
                <input
                  type="checkbox"
                  class="checkbox checkbox-xs"
                  checked={allSelected}
                  aria-label="Select all restore points"
                  onchange={(event) => toggleAll(event.currentTarget.checked)}
                />
              </th>
            {/if}
            <th>Created</th>
            <th class="hidden sm:table-cell">Duration</th>
            <th>Size</th>
            <th class="w-0"></th>
          </tr>
        </thead>
        <tbody>
          {#each restorePoints as point (point.snapshotId)}
            <tr class="hover:bg-base-200/60">
              {#if selecting}
                <td>
                  <input
                    type="checkbox"
                    class="checkbox checkbox-xs"
                    checked={selected.has(point.snapshotId)}
                    aria-label="Select restore point"
                    onchange={() =>
                      selected.has(point.snapshotId)
                        ? selected.delete(point.snapshotId)
                        : selected.add(point.snapshotId)}
                  />
                </td>
              {/if}
              <td title={formatDateTime(point.created)}>
                {relativeTime(point.created, $now)}
              </td>
              <td class="hidden text-base-content/60 sm:table-cell">{formatDuration(point.started, point.created)}</td>
              <td class="font-mono text-xs text-base-content/60">
                <div>{formatBytes(point.sizeBytes)}</div>
                {#if point.transferredBytes !== null}
                  <div class="text-[10px] text-base-content/40" title="Payload received from the phone during this backup">
                    {formatBytes(point.transferredBytes)} transferred
                  </div>
                {/if}
              </td>
              <td class="whitespace-nowrap text-right">
                {#if !selecting}
                  <button
                    type="button"
                    class="btn btn-ghost btn-xs"
                    disabled={!!restoreBlocked}
                    onclick={() => onrestore(point.snapshotId)}
                    title={restoreBlocked ?? 'Restore this snapshot onto the phone'}
                    aria-label="Restore this snapshot"
                  >
                    <Icon name="backup" size={13} />
                  </button>
                  <a
                    class="btn btn-ghost btn-xs"
                    href={backupDownloadUrl(point.snapshotId)}
                    download
                    title={`Download as a Finder backup (${formatBytes(point.sizeBytes)})`}
                    aria-label="Download this snapshot as a Finder backup"
                  >
                    <Icon name="download" size={13} />
                  </a>
                  <button
                    type="button"
                    class="btn btn-ghost btn-xs text-error"
                    disabled={isRunning}
                    onclick={() => askDelete([point])}
                    title={isRunning ? 'A backup or restore is running' : 'Delete this restore point'}
                    aria-label="Delete restore point"
                  >
                    <Icon name="trash" size={13} />
                  </button>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>

{#if pending.length > 0}
  <ConfirmDialog
    title={pending.length > 1 ? `Delete ${pending.length} restore points?` : 'Delete this restore point?'}
    icon="trash"
    confirmLabel="Delete"
    busyLabel="Deleting…"
    cancelLabel={pending.length > 1 ? 'Keep them' : 'Keep it'}
    failureCode="snapshot_delete_failed"
    onconfirm={confirmDelete}
    onclose={() => (pending = [])}
  >
    <p class="py-3 text-sm text-base-content/70">
      {#if pending.length > 1}
        The {pending.length} selected restore points will be removed. Other restore points are not
        affected.
      {:else}
        The backup from
        <span class="font-medium">
          {formatDateTime(pending[0].created)}
        </span>
        will be removed. Other restore points are not affected.
      {/if}
    </p>
    <p class="flex items-center gap-1.5 rounded-box bg-base-200 p-3 text-sm">
      <Icon name="info" size={14} />
      {#await reclaim}
        <span class="text-base-content/60">Calculating space freed…</span>
      {:then bytes}
        <span>Frees about <span class="font-medium tabular-nums">{formatBytes(bytes)}</span> on disk</span>
      {:catch}
        <span class="text-base-content/60">Space could not be calculated right now</span>
      {/await}
    </p>
  </ConfirmDialog>
{/if}
