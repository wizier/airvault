<script lang="ts">
  // Failed attempts are runtime events; this list contains only completed
  // restore points that still exist. One dialog serves single and multi
  // deletion; the freed-space estimate covers the whole selection.
  import { untrack } from 'svelte';
  import { SvelteSet } from 'svelte/reactivity';
  import { deleteSnapshots, snapshotsReclaimable, type RestorePoint } from '../api/backups';
  import { ApiError, errMsg, errRef, isAbortError } from '../api/client';
  import type { Device } from '../api/devices';
  import type { ErrorRef } from '../error-text';
  import { liveRun } from '../events.svelte';
  import { restorePointResources, restoreSourcesStore } from '../stores.svelte';
  import { formatBytes, formatDateTime, formatDuration, relativeTime } from '../format';
  import { now } from '../clock';
  import EmptyState from './EmptyState.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';

  let {
    device,
    onrestore,
  }: {
    device: Device;
    /** Open the restore flow with this snapshot preselected. */
    onrestore?: (snapshotId: string) => void;
  } = $props();

  const udid = $derived(device.udid);
  const reachable = $derived(device.connection !== 'offline');

  const points = $derived(restorePointResources.for(udid));
  $effect(() => points.start());
  const restorePoints = $derived(points.data ?? []);
  const pointsLoaded = $derived(points.ready);
  const pointsError = $derived(
    !points.ready && points.error && !(points.error instanceof ApiError && points.error.offline)
      ? errMsg(points.error, 'backup_history_failed')
      : null,
  );

  const isRunning = $derived(liveRun(udid) !== null);
  // Checkboxes appear only in selection mode; leaving it drops the selection.
  let selecting = $state(false);
  const selected = new SvelteSet<string>();
  let pending = $state<RestorePoint[]>([]);
  let reclaimable = $state<number | null>(null);
  let reclaiming = $state(false);
  let reclaimError = $state(false);
  let reclaimCtrl: AbortController | null = null;
  let deleting = $state(false);
  let deleteFailure = $state<ErrorRef | null>(null);
  let deleteDialog = $state<HTMLDialogElement | null>(null);

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
    reclaimCtrl?.abort();
    const ctrl = new AbortController();
    reclaimCtrl = ctrl;
    pending = targets;
    reclaimable = null;
    reclaiming = true;
    reclaimError = false;
    deleteFailure = null;
    deleteDialog?.showModal();
    // Honest "space freed": data no kept restore point still references.
    snapshotsReclaimable(udid, targets.map((point) => point.snapshotId), ctrl.signal)
      .then((bytes) => {
        if (reclaimCtrl === ctrl) reclaimable = bytes;
      })
      .catch((error) => {
        if (!isAbortError(error) && reclaimCtrl === ctrl) reclaimError = true;
      })
      .finally(() => {
        if (reclaimCtrl === ctrl) {
          reclaimCtrl = null;
          reclaiming = false;
        }
      });
  }

  function askDeleteSelected() {
    askDelete(selectedPoints);
  }

  function closeDeleteDialog() {
    reclaimCtrl?.abort();
    reclaimCtrl = null;
    reclaiming = false;
    pending = [];
  }

  async function confirmDelete() {
    if (pending.length === 0 || deleting) return;
    const ids = new Set(pending.map((point) => point.snapshotId));
    deleting = true;
    deleteFailure = null;
    try {
      await deleteSnapshots(udid, [...ids]);
      points.mutate((current) => current.filter((point) => !ids.has(point.snapshotId)));
      restoreSourcesStore.mutate((current) =>
        current.filter((source) => !ids.has(source.snapshotId)),
      );
      stopSelecting();
      deleteDialog?.close();
      pending = [];
      // Disk space frees in the background; a later backup.catalog event
      // refreshes this list and the device sizes.
    } catch (error) {
      deleteFailure = errRef(error, 'snapshot_delete_failed');
    } finally {
      deleting = false;
    }
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
            onclick={askDeleteSelected}
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

  {#if !pointsLoaded}
    <div class="flex items-center gap-3 rounded-box bg-base-100 p-4 text-sm text-base-content/60 shadow-sm">
      <span class="loading loading-spinner loading-sm"></span>
      Loading restore points…
    </div>
  {:else if pointsError}
    <div role="alert" class="alert alert-error alert-soft">
      <Icon name="alert" size={16} />
      <span>{pointsError}</span>
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
                    disabled={!reachable || isRunning}
                    onclick={() => onrestore?.(point.snapshotId)}
                    title={!reachable
                      ? 'Device is offline'
                      : isRunning
                        ? 'A backup or restore is running'
                        : 'Restore this snapshot onto the phone'}
                    aria-label="Restore this snapshot"
                  >
                    <Icon name="backup" size={13} />
                  </button>
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

<dialog
  class="modal"
  bind:this={deleteDialog}
  oncancel={(event) => deleting && event.preventDefault()}
  onclose={closeDeleteDialog}
>
  <div class="modal-box">
    <h3 class="text-lg font-bold">
      {pending.length > 1 ? `Delete ${pending.length} restore points?` : 'Delete this restore point?'}
    </h3>
    <p class="py-3 text-sm text-base-content/70">
      {#if pending.length > 1}
        The {pending.length} selected restore points will be removed. Other restore points are not
        affected.
      {:else if pending.length === 1}
        The backup from
        <span class="font-medium">
          {formatDateTime(pending[0].created)}
        </span>
        will be removed. Other restore points are not affected.
      {/if}
    </p>
    <p class="flex items-center gap-1.5 rounded-box bg-base-200 p-3 text-sm">
      <Icon name="info" size={14} />
      {#if reclaiming}
        <span class="text-base-content/60">Calculating space freed…</span>
      {:else if reclaimable !== null}
        <span>Frees about <span class="font-medium tabular-nums">{formatBytes(reclaimable)}</span> on disk</span>
      {:else if reclaimError}
        <span class="text-base-content/60">Space could not be calculated right now</span>
      {:else}
        <span class="text-base-content/60">No reclaim estimate is available.</span>
      {/if}
    </p>
    <ErrorLine failure={deleteFailure} className="mt-3" />
    <div class="modal-action">
      <button type="button" class="btn btn-ghost" disabled={deleting} onclick={() => deleteDialog?.close()}>
        {pending.length > 1 ? 'Keep them' : 'Keep it'}
      </button>
      <button type="button" class="btn btn-error" disabled={deleting} onclick={confirmDelete}>
        {#if deleting}
          <span class="loading loading-spinner loading-xs"></span> Deleting…
        {:else}
          <Icon name="trash" size={15} /> Delete
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close" disabled={deleting}>close</button>
  </form>
</dialog>
