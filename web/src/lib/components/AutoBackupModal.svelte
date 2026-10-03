<script lang="ts">
  import { untrack } from 'svelte';
  import { AUTO_BACKUP_PRESETS, setAutoBackup, type AutoBackupDays, type AutoBackupState } from '../api/devices';
  import { AUTO_BACKUP_EVERY } from '../device-ui';
  import type { ErrorTextKey } from '../error-text';
  import { Submit } from '../submit.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';

  let { udid, name, current, onclose }: {
    udid: string;
    name: string;
    /** The settings this open starts from; the modal mounts fresh per open. */
    current: AutoBackupState | undefined;
    onclose: () => void;
  } = $props();

  let modal: Modal;
  // Seeded once from the settings at open: a refetch must not reset the form.
  const initial = untrack(() => current);
  // Both "Set up…" and "Change…" open it meaning to have it on.
  let enabled = $state(true);
  let everyDays = $state<AutoBackupDays>(initial?.everyDays ?? 1);
  let windowOn = $state(!!initial?.window);
  let start = $state(initial?.window?.start ?? '19:00');
  let end = $state(initial?.window?.end ?? '23:00');
  const submit = new Submit('auto_backup_save_failed');

  // The server checks the window whenever one is sent, enabled or not.
  const validationError = $derived.by<ErrorTextKey | null>(() =>
    windowOn && (!start || !end || start === end) ? 'invalid_auto_backup_window' : null,
  );

  function close() {
    submit.abort();
    onclose();
  }

  async function save() {
    if (validationError) {
      submit.code = validationError;
      return;
    }
    const settings = { enabled, everyDays, window: windowOn ? { start, end } : undefined };
    const ok = await submit.run((signal) => setAutoBackup(udid, settings, signal));
    if (ok) modal.close();
  }
</script>

<Modal bind:this={modal} title={`Automatic backup of ${name}`} onclose={close}>
  <p class="py-3 text-sm text-base-content/70">
    iOS asks for the iPhone passcode before every backup and closes the prompt after about a
    minute, so a backup can't run unattended. AirVault starts one about 5 seconds after the
    iPhone is unlocked at home on Wi-Fi — while it's in your hands — so you just enter the
    passcode when asked.
  </p>

  <div class="flex flex-col gap-3">
    <label class="label cursor-pointer gap-3 rounded-box bg-base-200 p-3">
      <input type="checkbox" class="checkbox checkbox-sm" bind:checked={enabled} disabled={submit.busy} />
      <span class="text-sm leading-snug">
        <span>Back up automatically</span>
        <span class="mt-0.5 block text-xs text-base-content/60">
          An ignored prompt pauses it for an hour, at most three tries a day
        </span>
      </span>
    </label>

    <label class="flex flex-col gap-1.5">
      <span class="label">How often</span>
      <select class="select w-full" bind:value={everyDays} disabled={submit.busy}>
        {#each AUTO_BACKUP_PRESETS as days (days)}
          <option value={days}>{AUTO_BACKUP_EVERY[days]}</option>
        {/each}
      </select>
    </label>

    <div class="flex flex-col gap-2">
      <label class="label cursor-pointer gap-3">
        <input type="checkbox" class="checkbox checkbox-sm" bind:checked={windowOn} disabled={submit.busy} />
        <span class="text-sm">Only between</span>
      </label>
      {#if windowOn}
        <div class="flex items-center gap-2 pl-7">
          <input type="time" class="input input-sm w-32" bind:value={start} disabled={submit.busy} aria-label="From" />
          <span class="text-base-content/60">–</span>
          <input type="time" class="input input-sm w-32" bind:value={end} disabled={submit.busy} aria-label="Until" />
        </div>
        <p class="pl-7 text-xs text-base-content/60">
          Server time ({initial?.timeZone}); the window may cross midnight
        </p>
      {/if}
    </div>
  </div>

  <ErrorLine error={submit.failure} className="mt-3" />

  {#snippet actions()}
    <button type="button" class="btn btn-ghost" onclick={() => modal.close()}>Cancel</button>
    <button type="button" class="btn btn-primary" disabled={submit.busy} onclick={save}>
      {#if submit.busy}
        <span class="loading loading-spinner loading-xs"></span>
        Saving…
      {:else}
        <Icon name="clock" size={15} /> Save
      {/if}
    </button>
  {/snippet}
</Modal>
