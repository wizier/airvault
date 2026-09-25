<script lang="ts">
  // Backup-encryption password management (iOS "Encrypt local backup"):
  // enable = new password, change = old + new, disable = old only. The request
  // stays in the modal so device verdicts can be corrected and retried in place.
  import { errorCode, isAbortError } from '../api/client';
  import { changeBackupPassword } from '../api/devices';
  import { errorText, type ErrorTextKey } from '../error-text';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import PasswordField from './PasswordField.svelte';

  export type PasswordMode = 'enable' | 'change' | 'disable';

  let { udid, name, mode, onclose }: {
    udid: string;
    name: string;
    mode: PasswordMode;
    onclose: () => void;
  } = $props();

  let dialog: HTMLDialogElement;
  let oldPw = $state('');
  let newPw = $state('');
  let confirmPw = $state('');
  let busy = $state(false);
  let failureCode = $state<string | null>(null);
  const failure = $derived(failureCode && errorText(failureCode, 'backup_password_change_failed'));
  let requestController: AbortController | null = null;

  const title = $derived(
    mode === 'enable'
      ? `Encrypt backups of ${name}`
      : mode === 'change'
        ? `Change the backup password of ${name}`
        : `Turn off backup encryption for ${name}`,
  );

  const needsOld = $derived(mode === 'change' || mode === 'disable');
  const needsNew = $derived(mode === 'enable' || mode === 'change');

  const validationError = $derived.by<ErrorTextKey | null>(() => {
    if (needsOld && !oldPw) return 'current_backup_password_required';
    if (needsNew && !newPw) return 'new_backup_password_required';
    if (needsNew && newPw !== confirmPw) return 'backup_passwords_do_not_match';
    return null;
  });

  // Device verdicts are stable service codes; UI copy is never parsed.
  const deviceLocked = $derived(failureCode === 'device_locked' && !validationError);
  const wrongPassword = $derived(
    needsOld && failureCode === 'invalid_backup_password' && !validationError,
  );

  function close() {
    requestController?.abort();
    onclose();
  }

  async function submit() {
    if (busy) return;
    if (validationError) {
      failureCode = validationError;
      return;
    }
    busy = true;
    failureCode = null;
    const controller = new AbortController();
    requestController = controller;
    try {
      await changeBackupPassword(
        udid,
        needsOld ? oldPw : '',
        needsNew ? newPw : '',
        controller.signal,
      );
      dialog.close();
    } catch (err) {
      if (isAbortError(err)) return;
      failureCode = errorCode(err, 'backup_password_change_failed');
    } finally {
      if (requestController === controller) requestController = null;
      busy = false;
    }
  }
</script>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} onclose={close}>
  <div class="modal-box">
    <h3 class="text-lg font-bold">{title}</h3>

    {#if mode === 'enable'}
      <p class="py-3 text-sm text-base-content/70">
        Encrypted backups include Health and Keychain data. The password lives on the phone and
        protects every future backup.
      </p>
      <div role="alert" class="alert alert-warning alert-soft">
        <Icon name="alert" size={18} />
        <p class="text-sm">
          <span class="font-medium">The password cannot be recovered.</span> Without it you can't
          restore an encrypted backup.
        </p>
      </div>
    {:else if mode === 'disable'}
      <p class="py-3 text-sm text-base-content/70">
        Future backups will no longer include Health and Keychain data. You need the current
        password to turn encryption off.
      </p>
    {:else}
      <p class="py-3 text-sm text-base-content/70">
        Enter the current password, then the new one. The change applies to all future backups.
      </p>
    {/if}

    <div class="mt-3 flex flex-col gap-3">
      {#if needsOld}
        <PasswordField
          label="Current password"
          bind:value={oldPw}
          autocomplete="current-password"
          disabled={busy}
          error={wrongPassword ? "This doesn't match the backup password on the phone" : null}
          focus={wrongPassword}
        />
      {/if}
      {#if needsNew}
        <PasswordField label="New password" bind:value={newPw} autocomplete="new-password" disabled={busy} />
        <PasswordField label="Confirm new password" bind:value={confirmPw} autocomplete="new-password" disabled={busy} />
      {/if}
    </div>

    {#if busy}
      <div class="mt-4 flex items-start gap-3 rounded-box bg-base-200 p-4">
        <span class="flex h-10 w-10 shrink-0 items-center justify-center rounded-box bg-primary/10 text-primary">
          <Icon name="phone" size={20} />
        </span>
        <div class="min-w-0">
          <p class="text-sm font-semibold">Now check the iPhone</p>
          <p class="mt-0.5 text-sm text-base-content/70">
            Confirm the device passcode there if asked. The password update normally completes
            within a few seconds after confirmation.
          </p>
        </div>
        <span class="loading loading-spinner loading-sm ml-auto shrink-0 self-center"></span>
      </div>
    {:else if deviceLocked}
      <div role="alert" class="alert alert-warning alert-soft mt-3">
        <Icon name="lock" size={18} />
        <div class="text-sm">
          <p class="font-medium">The iPhone is locked</p>
          <p class="mt-1 opacity-80">Unlock it (keep it unlocked) and try again.</p>
        </div>
      </div>
    {:else if !wrongPassword}
      <ErrorLine error={failure} className="mt-3" />
    {/if}

    <div class="modal-action">
      <button type="button" class="btn btn-ghost" onclick={() => dialog.close()}>Cancel</button>
      <button
        type="button"
        class={`btn ${mode === 'disable' ? 'btn-error' : 'btn-primary'}`}
        disabled={busy}
        onclick={submit}
      >
        {#if busy}
          <span class="loading loading-spinner loading-xs"></span>
          Applying…
        {:else if mode === 'enable'}
          <Icon name="lock" size={15} /> Enable encryption
        {:else if mode === 'change'}
          <Icon name="lock" size={15} /> Change password
        {:else}
          <Icon name="lock" size={15} /> Turn off encryption
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
