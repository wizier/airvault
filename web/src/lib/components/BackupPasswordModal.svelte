<script lang="ts">
  // enable = new password, change = old + new, disable = old only. The request
  // stays in the modal so device verdicts can be corrected and retried in place.
  import { changeBackupPassword } from '../api/devices';
  import type { ErrorTextKey } from '../error-text';
  import { Submit } from '../submit.svelte';
  import Alert from './Alert.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import PasswordField from './PasswordField.svelte';

  export type PasswordMode = 'enable' | 'change' | 'disable';

  let { udid, name, mode, onclose }: {
    udid: string;
    name: string;
    mode: PasswordMode;
    onclose: () => void;
  } = $props();

  let modal: Modal;
  let oldPw = $state('');
  let newPw = $state('');
  let confirmPw = $state('');
  const submit = new Submit('backup_password_change_failed');

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
  const deviceLocked = $derived(submit.code === 'device_locked' && !validationError);
  const wrongPassword = $derived(
    needsOld && submit.code === 'invalid_backup_password' && !validationError,
  );

  function close() {
    submit.abort();
    onclose();
  }

  async function apply() {
    if (validationError) {
      submit.code = validationError;
      return;
    }
    const oldPassword = needsOld ? oldPw : '';
    const newPassword = needsNew ? newPw : '';
    const ok = await submit.run((signal) => changeBackupPassword(udid, oldPassword, newPassword, signal));
    if (ok) modal.close();
  }
</script>

<Modal bind:this={modal} {title} onclose={close}>
  {#if mode === 'enable'}
    <p class="py-3 text-sm text-base-content/70">
      Encrypted backups include Health and Keychain data. The password lives on the phone and
      protects every future backup.
    </p>
    <Alert tone="warning" class="text-sm">
      <span class="font-medium">The password cannot be recovered.</span> Without it you can't
      restore an encrypted backup.
    </Alert>
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
        disabled={submit.busy}
        error={wrongPassword ? "This doesn't match the backup password on the phone" : null}
        focus={wrongPassword}
      />
    {/if}
    {#if needsNew}
      <PasswordField label="New password" bind:value={newPw} autocomplete="new-password" disabled={submit.busy} />
      <PasswordField label="Confirm new password" bind:value={confirmPw} autocomplete="new-password" disabled={submit.busy} />
    {/if}
  </div>

  {#if submit.busy}
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
    <Alert tone="warning" icon="lock" title="The iPhone is locked" class="mt-3 text-sm">
      Unlock it (keep it unlocked) and try again.
    </Alert>
  {:else if !wrongPassword}
    <ErrorLine error={submit.failure} className="mt-3" />
  {/if}

  {#snippet actions()}
    <button type="button" class="btn btn-ghost" onclick={() => modal.close()}>Cancel</button>
    <button
      type="button"
      class={`btn ${mode === 'disable' ? 'btn-error' : 'btn-primary'}`}
      disabled={submit.busy}
      onclick={apply}
    >
      {#if submit.busy}
        <span class="loading loading-spinner loading-xs"></span>
        Applying…
      {:else if mode === 'enable'}
        <Icon name="lock" size={15} /> Enable encryption
      {:else if mode === 'change'}
        <Icon name="key" size={15} /> Change password
      {:else}
        <Icon name="unlocked" size={15} /> Turn off encryption
      {/if}
    </button>
  {/snippet}
</Modal>
