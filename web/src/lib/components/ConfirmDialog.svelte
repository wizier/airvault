<script lang="ts">
  // Confirmation modal for one async action, open while mounted. It owns the
  // busy and failure state; a successful action closes it.
  import type { Snippet } from 'svelte';
  import { errMsg } from '../api/client';
  import type { ErrorTextKey } from '../error-text';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import type { IconName } from './icons';

  let {
    title,
    icon,
    confirmLabel,
    busyLabel,
    cancelLabel = 'Cancel',
    confirmClass = 'btn-error',
    errorClass = 'mt-3',
    failureCode,
    onconfirm,
    onclose,
    children,
  }: {
    title: string;
    icon: IconName;
    confirmLabel: string;
    busyLabel: string;
    cancelLabel?: string;
    confirmClass?: string;
    errorClass?: string;
    failureCode: ErrorTextKey;
    onconfirm: () => Promise<void>;
    onclose: () => void;
    children: Snippet;
  } = $props();

  let dialog: HTMLDialogElement;
  let busy = $state(false);
  let error = $state<string | null>(null);

  async function confirm() {
    busy = true;
    error = null;
    try {
      await onconfirm();
      dialog.close();
    } catch (err) {
      error = errMsg(err, failureCode);
    } finally {
      busy = false;
    }
  }
</script>

<dialog
  class="modal"
  bind:this={dialog}
  {@attach (d) => d.showModal()}
  oncancel={(event) => busy && event.preventDefault()}
  {onclose}
>
  <div class="modal-box">
    <h3 class="text-lg font-bold">{title}</h3>
    {@render children()}
    <ErrorLine {error} className={errorClass} />
    <div class="modal-action">
      <button type="button" class="btn btn-ghost" disabled={busy} onclick={() => dialog.close()}>{cancelLabel}</button>
      <button type="button" class={`btn ${confirmClass}`} disabled={busy} onclick={confirm}>
        {#if busy}
          <span class="loading loading-spinner loading-xs"></span> {busyLabel}
        {:else}
          <Icon name={icon} size={15} /> {confirmLabel}
        {/if}
      </button>
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close" disabled={busy}>close</button>
  </form>
</dialog>
