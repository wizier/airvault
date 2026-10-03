<script lang="ts">
  import { unlockBackup } from '../api/backup-contents';
  import type { RestorePoint } from '../api/backups';
  import { formatDateTime } from '../format';
  import { Submit } from '../submit.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import PasswordField from './PasswordField.svelte';

  let { point, onclose }: {
    point: RestorePoint;
    /** True once the server holds the backup unlocked. */
    onclose: (unlocked: boolean) => void;
  } = $props();

  let modal: Modal;
  let password = $state('');
  let unlocked = false;
  const submit = new Submit('backup_unlock_failed');
  const wrongPassword = $derived(submit.code === 'invalid_backup_password');

  async function unlock(event: SubmitEvent) {
    event.preventDefault();
    if (!password) {
      submit.code = 'backup_password_required';
      return;
    }
    unlocked = await submit.run((signal) => unlockBackup(point.snapshotId, password, signal));
    if (unlocked) modal.close();
  }

  function close() {
    submit.abort();
    onclose(unlocked);
  }
</script>

<Modal bind:this={modal} title="Unlock the backup" subtitle={formatDateTime(point.created)} onclose={close}>
  <form id="unlock-backup" class="flex flex-col gap-3 py-3" onsubmit={unlock}>
    <p class="text-sm text-base-content/70">
      This backup is encrypted. Enter its password to browse its files.
    </p>
    <PasswordField
      label="Backup password"
      bind:value={password}
      autocomplete="current-password"
      disabled={submit.busy}
      error={wrongPassword ? 'That password does not unlock this backup. Try again.' : null}
      focus
    />
  </form>
  {#if !wrongPassword}
    <ErrorLine error={submit.failure} />
  {/if}

  {#snippet actions()}
    <button type="button" class="btn btn-ghost" onclick={() => modal.close()}>Cancel</button>
    <button type="submit" form="unlock-backup" class="btn btn-primary" disabled={submit.busy}>
      {#if submit.busy}
        <span class="loading loading-spinner loading-xs"></span>
        Unlocking…
      {:else}
        <Icon name="unlocked" size={15} /> Unlock
      {/if}
    </button>
  {/snippet}
</Modal>
