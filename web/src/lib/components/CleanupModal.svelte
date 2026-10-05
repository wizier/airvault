<script lang="ts">
  import { untrack } from 'svelte';
  import { snapshotsReclaimable } from '../api/backups';
  import { errMsg } from '../api/client';
  import {
    CLEANUP_DAY_PRESETS,
    planCleanup,
    setCleanup,
    type CleanupDays,
    type CleanupSettings,
    type ThinPeriod,
  } from '../api/devices';
  import { CLEANUP_THIN } from '../device-ui';
  import { errorText } from '../error-text';
  import { formatBytes } from '../format';
  import { deleteRestorePoints, restorePointResources } from '../stores.svelte';
  import { Submit } from '../submit.svelte';
  import CleanupTimeline from './CleanupTimeline.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';

  let { udid, name, current, onclose }: {
    udid: string;
    name: string;
    /** The settings this open starts from; the modal mounts fresh per open. */
    current: CleanupSettings | undefined;
    onclose: () => void;
  } = $props();

  let modal: Modal;
  // Seeded once from the settings at open: a refetch must not reset the form.
  const initial = untrack(() => current);
  // Both "Set up…" and "Change…" open it meaning to have it on.
  let enabled = $state(true);
  let keepDays = $state<CleanupDays>(initial?.keepDays ?? 14);
  let thin = $state<ThinPeriod>(initial?.thin ?? 'month');
  const submit = new Submit('cleanup_save_failed');

  const points = $derived(restorePointResources.for(udid));
  $effect(() => points.start());
  const listed = $derived(points.data ?? []);

  // The plan stays on screen while the next one loads, so the line doesn't jump.
  let plan = $state<string[] | null>(null);
  let planning = $state(false);
  let planError = $state<string | null>(null);
  $effect(() => {
    if (!enabled) return;
    const ctrl = new AbortController();
    planning = true;
    planError = null;
    planCleanup(udid, { keepDays, thin }, ctrl.signal)
      .then((next) => (plan = next))
      .catch((err) => {
        if (!ctrl.signal.aborted) planError = errMsg(err, 'cleanup_plan_failed');
      })
      .finally(() => {
        if (!ctrl.signal.aborted) planning = false;
      });
    return () => ctrl.abort();
  });

  // Listed points only: the list may have moved on since the plan.
  const removing = $derived.by(() => {
    const planned = new Set(plan);
    return listed.filter((point) => planned.has(point.snapshotId)).map((point) => point.snapshotId);
  });
  const remove = $derived(new Set(removing));

  // The size reads every manifest, so it follows the plan and never holds Save.
  let freed = $state<Promise<number>>();
  $effect(() => {
    if (removing.length === 0) return;
    const ctrl = new AbortController();
    freed = snapshotsReclaimable(udid, removing, ctrl.signal);
    return () => ctrl.abort();
  });

  // Saving removes what the preview shows the way deleting selected restore
  // points does; after that, each backup applies the settings.
  let deleting = $state(false);
  let deleteError = $state<string | null>(null);
  const busy = $derived(submit.busy || deleting);

  async function save() {
    const ids = enabled ? removing : [];
    deleteError = null;
    if (!(await submit.run((signal) => setCleanup(udid, { enabled, keepDays, thin }, signal)))) return;
    if (ids.length > 0) {
      deleting = true;
      try {
        await deleteRestorePoints(udid, ids);
      } catch {
        deleteError = errorText('cleanup_remove_failed');
        return;
      } finally {
        deleting = false;
      }
    }
    modal.close();
  }
</script>

<Modal bind:this={modal} title={`Automatic cleanup of ${name}`} locked={busy} {onclose}>
  <div class="flex flex-col gap-4 py-3">
    <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
      <input type="checkbox" class="checkbox checkbox-sm" bind:checked={enabled} disabled={busy} />
      <span class="whitespace-normal text-sm leading-snug">
        <span>Remove old backups automatically</span>
        <span class="mt-0.5 block text-xs text-base-content/60">
          Runs after each backup. Damaged backups are never removed.
        </span>
      </span>
    </label>

    <div class="flex flex-col gap-3 text-sm" class:opacity-50={!enabled}>
      <div class="flex flex-col gap-1">
        <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span>Keep every backup from the last</span>
          <select
            class="select select-sm w-20"
            bind:value={keepDays}
            disabled={!enabled || busy}
            aria-label="How many of the last days to keep every backup from"
          >
            {#each CLEANUP_DAY_PRESETS as count (count)}
              <option value={count}>{count}</option>
            {/each}
          </select>
          <span>days</span>
        </div>
        <p class="text-xs text-base-content/60">If the phone stops backing up, its last backups are kept</p>
      </div>

      <div class="flex flex-col gap-1">
        <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span>Older backups:</span>
          <select
            class="select select-sm w-44"
            bind:value={thin}
            disabled={!enabled || busy}
            aria-label="What to keep of older backups"
          >
            {#each Object.entries(CLEANUP_THIN) as [period, copy] (period)}
              <option value={period}>{copy.label}</option>
            {/each}
          </select>
        </div>
        <p class="text-xs text-base-content/60">{CLEANUP_THIN[thin].hint}</p>
      </div>
    </div>

    {#if enabled}
      {#if planError}
        <ErrorLine error={planError} size="xs" />
      {:else if plan === null || !points.ready}
        <p class="flex items-center gap-2 text-xs text-base-content/60">
          <span class="loading loading-spinner loading-xs"></span>
          Working out which backups stay…
        </p>
      {:else if listed.length > 0}
        <div class="flex flex-col gap-2 transition-opacity" class:opacity-60={planning}>
          <CleanupTimeline points={listed} {remove} />
          <p class="flex items-start gap-1.5 text-sm">
            <Icon name="info" size={14} class="mt-0.5 shrink-0" />
            {#if removing.length === 0}
              <span>Nothing to remove yet: all {listed.length} backups stay.</span>
            {:else}
              <span>
                Keeps {listed.length - removing.length} of {listed.length} backups and removes {removing.length}.
                {#await freed}
                  <span class="text-base-content/60">Calculating space freed…</span>
                {:then bytes}
                  Frees about <span class="font-medium tabular-nums">{formatBytes(bytes)}</span>.
                {:catch}{/await}
              </span>
            {/if}
          </p>
        </div>
      {/if}
    {/if}
  </div>

  <ErrorLine error={submit.failure ?? deleteError} className="mt-1" />

  {#snippet actions()}
    <button type="button" class="btn btn-ghost" disabled={busy} onclick={() => modal.close()}>Cancel</button>
    <button
      type="button"
      class={`btn ${enabled && removing.length > 0 ? 'btn-error' : 'btn-primary'}`}
      disabled={busy || (enabled && (planning || plan === null) && !planError)}
      onclick={save}
    >
      {#if busy}
        <span class="loading loading-spinner loading-xs"></span>
        {deleting ? 'Removing…' : 'Saving…'}
      {:else}
        <Icon name="history" size={15} />
        {enabled && removing.length > 0 ? `Save and remove ${removing.length}` : 'Save'}
      {/if}
    </button>
  {/snippet}
</Modal>
