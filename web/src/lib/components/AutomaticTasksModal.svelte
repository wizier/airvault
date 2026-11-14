<script lang="ts">
  import { untrack } from 'svelte';
  import { snapshotsReclaimable } from '../api/backups';
  import { errMsg } from '../api/client';
  import {
    AUTO_BACKUP_PRESETS,
    AUTO_VERIFY_PRESETS,
    CLEANUP_DAY_PRESETS,
    planCleanup,
    setAutomaticTasks,
    type AutoBackupDays,
    type AutoVerifyDays,
    type CleanupDays,
    type Device,
    type ThinPeriod,
  } from '../api/devices';
  import { AUTO_BACKUP_EVERY, AUTO_VERIFY_EVERY, CLEANUP_THIN, deviceNoun } from '../device-ui';
  import { errorText } from '../error-text';
  import { formatBytes } from '../format';
  import { deleteRestorePoints, restorePointsOf, restorePointsStore } from '../stores.svelte';
  import { Submit } from '../submit.svelte';
  import AutomaticTask from './AutomaticTask.svelte';
  import CleanupTimeline from './CleanupTimeline.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import ScheduleFields, { scheduleForm, scheduleSettings, windowInvalid } from './ScheduleFields.svelte';

  let { device, onclose }: { device: Device; onclose: () => void } = $props();

  const noun = $derived(deviceNoun(device.productType));

  let modal: Modal;
  // Seeded once from the settings at open: a refetch must not reset the form.
  const { udid, name, paired, autoBackup, cleanup, autoVerify } = untrack(() => device);

  let backup = $state(scheduleForm<AutoBackupDays>(autoBackup, { everyDays: 1, start: '19:00', end: '23:00' }));
  let verify = $state(scheduleForm<AutoVerifyDays>(autoVerify, { everyDays: 7, start: '02:00', end: '06:00' }));

  let cleanupOn = $state(cleanup?.enabled ?? false);
  let keepDays = $state<CleanupDays>(cleanup?.keepDays ?? 14);
  let thin = $state<ThinPeriod>(cleanup?.thin ?? 'month');

  const submit = new Submit('automatic_tasks_save_failed');

  $effect(() => restorePointsStore.start());
  const listed = $derived(restorePointsOf(udid));

  // The plan stays on screen while the next one loads, so the line doesn't jump.
  let plan = $state<string[] | null>(null);
  let planning = $state(false);
  let planError = $state<string | null>(null);
  $effect(() => {
    if (!cleanupOn) return;
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
    if (!cleanupOn) return [];
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
    if (windowInvalid(backup) || windowInvalid(verify)) {
      submit.code = 'invalid_auto_window';
      return;
    }
    const ids = removing;
    deleteError = null;
    const saved = await submit.run((signal) =>
      setAutomaticTasks(
        udid,
        {
          autoBackup: scheduleSettings(backup),
          cleanup: { enabled: cleanupOn, keepDays, thin },
          autoVerify: scheduleSettings(verify),
        },
        signal,
      ),
    );
    if (!saved) return;
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

<Modal bind:this={modal} title={`Automatic tasks of ${name}`} size="medium" locked={busy} {onclose}>
  <div class="-mx-1 flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto px-1 py-3 outline-none">
    <AutomaticTask
      title="Back up automatically"
      hint={paired
        ? `Starts when you unlock the ${noun} at home on Wi-Fi. iOS asks for the passcode each time.`
        : `Pair the ${noun} over USB first`}
      bind:on={backup.on}
      disabled={busy || !paired}
    >
      <ScheduleFields
        presets={AUTO_BACKUP_PRESETS}
        labels={AUTO_BACKUP_EVERY}
        bind:form={backup}
        timeZone={autoBackup?.timeZone}
        disabled={busy || !paired}
      />
    </AutomaticTask>

    <AutomaticTask
      title="Remove old backups automatically"
      hint="Runs after each backup. Damaged backups are never removed."
      bind:on={cleanupOn}
      disabled={busy}
    >
      <div class="flex flex-col gap-1">
        <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span>Keep every backup from the last</span>
          <select
            class="select select-sm w-20"
            bind:value={keepDays}
            disabled={busy}
            aria-label="How many of the last days to keep every backup from"
          >
            {#each CLEANUP_DAY_PRESETS as count (count)}
              <option value={count}>{count}</option>
            {/each}
          </select>
          <span>days</span>
        </div>
        <p class="text-xs text-base-content/60">If the {noun} stops backing up, its last backups are kept</p>
      </div>

      <div class="flex flex-col gap-1">
        <div class="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span>Older backups:</span>
          <select
            class="select select-sm w-44"
            bind:value={thin}
            disabled={busy}
            aria-label="What to keep of older backups"
          >
            {#each Object.entries(CLEANUP_THIN) as [period, copy] (period)}
              <option value={period}>{copy.label}</option>
            {/each}
          </select>
        </div>
        <p class="text-xs text-base-content/60">{CLEANUP_THIN[thin].hint}</p>
      </div>

      {#if planError}
        <ErrorLine error={planError} size="xs" />
      {:else if plan === null || !restorePointsStore.ready}
        <p class="flex items-center gap-2 text-xs text-base-content/60">
          <span class="loading loading-spinner loading-xs"></span>
          Working out which backups stay…
        </p>
      {:else if listed.length > 0}
        <div class="flex flex-col gap-2 transition-opacity" class:opacity-60={planning}>
          <CleanupTimeline points={listed} {remove} />
          <p class="flex items-start gap-1.5">
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
    </AutomaticTask>

    <AutomaticTask
      title="Check integrity automatically"
      hint={`Doesn't need the ${noun}. While it runs, this ${noun} can't back up, restore or download.`}
      bind:on={verify.on}
      disabled={busy}
    >
      <ScheduleFields
        presets={AUTO_VERIFY_PRESETS}
        labels={AUTO_VERIFY_EVERY}
        bind:form={verify}
        timeZone={autoVerify?.timeZone}
        disabled={busy}
      />
    </AutomaticTask>
  </div>

  <ErrorLine error={submit.failure ?? deleteError} className="mt-1 shrink-0" />

  {#snippet actions()}
    <button type="button" class="btn btn-ghost" disabled={busy} onclick={() => modal.close()}>Cancel</button>
    <button
      type="button"
      class={`btn ${removing.length > 0 ? 'btn-error' : 'btn-primary'}`}
      disabled={busy || (cleanupOn && (planning || plan === null) && !planError)}
      onclick={save}
    >
      {#if busy}
        <span class="loading loading-spinner loading-xs"></span>
        {deleting ? 'Removing…' : 'Saving…'}
      {:else}
        <Icon name="clock" size={15} />
        {removing.length > 0 ? `Save and remove ${removing.length}` : 'Save'}
      {/if}
    </button>
  {/snippet}
</Modal>
