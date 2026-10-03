<script lang="ts">
  // Open while mounted; it owns busy and failure state, and a successful action
  // closes it.
  import { untrack, type Snippet } from 'svelte';
  import type { ErrorTextKey } from '../error-text';
  import { Submit } from '../submit.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import type { IconName } from './icons';
  import Modal from './Modal.svelte';

  let {
    title,
    icon,
    confirmLabel,
    busyLabel,
    cancelLabel = 'Cancel',
    confirmClass = 'btn-error',
    confirmDisabled = false,
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
    /** Holds the action back until the dialog's own guard is met. */
    confirmDisabled?: boolean;
    failureCode: ErrorTextKey;
    onconfirm: () => Promise<void>;
    onclose: () => void;
    children: Snippet;
  } = $props();

  let modal: Modal;
  // The failure fallback is fixed for the dialog's life.
  const submit = new Submit(untrack(() => failureCode));

  async function confirm() {
    if (await submit.run(onconfirm)) modal.close();
  }
</script>

<Modal bind:this={modal} {title} locked={submit.busy} {onclose}>
  {@render children()}
  <ErrorLine error={submit.failure} className="mt-3" />
  {#snippet actions()}
    <button type="button" class="btn btn-ghost" disabled={submit.busy} onclick={() => modal.close()}>{cancelLabel}</button>
    <button type="button" class={`btn ${confirmClass}`} disabled={submit.busy || confirmDisabled} onclick={confirm}>
      {#if submit.busy}
        <span class="loading loading-spinner loading-xs"></span> {busyLabel}
      {:else}
        <Icon name={icon} size={15} /> {confirmLabel}
      {/if}
    </button>
  {/snippet}
</Modal>
