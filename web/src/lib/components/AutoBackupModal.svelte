<script lang="ts">
  import { untrack } from 'svelte';
  import { errorCode, isAbortError } from '../api/client';
  import { AUTO_BACKUP_PRESETS, setAutoBackup, type AutoBackupDays, type AutoBackupState } from '../api/devices';
  import { AUTO_BACKUP_EVERY, browserTimeZone } from '../device-ui';
  import { errorText, type ErrorTextKey } from '../error-text';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';

  let { udid, name, current, onclose }: {
    udid: string;
    name: string;
    /** The settings this open starts from; the modal mounts fresh per open. */
    current: AutoBackupState | undefined;
    onclose: () => void;
  } = $props();

  let dialog: HTMLDialogElement;
  // Seeded once from the settings at open: a refetch must not reset the form.
  const initial = untrack(() => current);
  // Both "Set up…" and "Change…" open it meaning to have it on.
  let enabled = $state(true);
  let everyDays = $state<AutoBackupDays>(initial?.everyDays ?? 1);
  let windowOn = $state(!!initial?.window);
  let start = $state(initial?.window?.start ?? '19:00');
  let end = $state(initial?.window?.end ?? '23:00');
  const timeZone = browserTimeZone();
  let busy = $state(false);
  let failureCode = $state<string | null>(null);
  const failure = $derived(failureCode && errorText(failureCode, 'auto_backup_save_failed'));
  let requestController: AbortController | null = null;

  // The server checks the window whenever one is sent, enabled or not.
  const validationError = $derived.by<ErrorTextKey | null>(() =>
    windowOn && (!start || !end || start === end) ? 'invalid_auto_backup_window' : null,
  );

  function close() {
    requestController?.abort();
    onclose();
  }

  async function submit() {
    if (validationError) {
      failureCode = validationError;
      return;
    }
    busy = true;
    failureCode = null;
    const controller = new AbortController();
    requestController = controller;
    try {
      await setAutoBackup(
        udid,
        { enabled, everyDays, window: windowOn ? { start, end, timeZone } : undefined },
        controller.signal,
      );
      dialog.close();
    } catch (err) {
      if (isAbortError(err)) return;
      failureCode = errorCode(err, 'auto_backup_save_failed');
    } finally {
      if (requestController === controller) requestController = null;
      busy = false;
    }
  }
</script>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} onclose={close}>
  <div class="modal-box">
    <h3 class="text-lg font-bold">Automatic backup of {name}</h3>

    <p class="py-3 text-sm text-base-content/70">
      iOS asks for the iPhone passcode before every backup and closes the prompt after about a
      minute, so a backup can't run unattended. AirVault starts one about 10 seconds after the
      iPhone is unlocked at home on Wi-Fi — while it's in your hands — so you just enter the
      passcode when asked.
    </p>

    <div class="flex flex-col gap-3">
      <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
        <input type="checkbox" class="checkbox checkbox-sm" bind:checked={enabled} disabled={busy} />
        <span class="text-sm leading-snug">
          <span>Back up automatically</span>
          <span class="mt-0.5 block text-xs text-base-content/60">
            An ignored prompt pauses it for an hour, at most three tries a day
          </span>
        </span>
      </label>

      <label class="flex flex-col gap-1.5">
        <span class="label">How often</span>
        <select class="select w-full" bind:value={everyDays} disabled={busy}>
          {#each AUTO_BACKUP_PRESETS as days (days)}
            <option value={days}>{AUTO_BACKUP_EVERY[days]}</option>
          {/each}
        </select>
      </label>

      <div class="flex flex-col gap-2">
        <label class="label cursor-pointer gap-3">
          <input type="checkbox" class="checkbox checkbox-sm" bind:checked={windowOn} disabled={busy} />
          <span class="text-sm">Only between</span>
        </label>
        {#if windowOn}
          <div class="flex items-center gap-2 pl-7">
            <input type="time" class="input input-sm w-32" bind:value={start} disabled={busy} aria-label="From" />
            <span class="text-base-content/60">–</span>
            <input type="time" class="input input-sm w-32" bind:value={end} disabled={busy} aria-label="Until" />
          </div>
          <p class="pl-7 text-xs text-base-content/60">
            Times are in {timeZone}; the window may cross midnight
          </p>
        {/if}
      </div>
    </div>

    <ErrorLine error={failure} className="mt-3" />

    <div class="modal-action">
      <button type="button" class="btn btn-ghost" onclick={() => dialog.close()}>Cancel</button>
      <button type="button" class="btn btn-primary" disabled={busy} onclick={submit}>
        {#if busy}
          <span class="loading loading-spinner loading-xs"></span>
          Saving…
        {:else}
          <Icon name="clock" size={15} /> Save
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
