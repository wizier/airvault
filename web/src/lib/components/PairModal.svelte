<script lang="ts">
  // Modal host for the pairing wizard. It is mounted only while open, so the
  // wizard's one-shot pair-state fetch and SSE reactions are scoped to the
  // visible flow.
  import { push } from 'svelte-spa-router';
  import PairWizard from './PairWizard.svelte';

  let { onclose }: { onclose: () => void } = $props();

  let dialog: HTMLDialogElement;

  function goDevice(udid: string) {
    dialog.close();
    push(`/device/${encodeURIComponent(udid)}`);
  }
</script>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} {onclose}>
  <div class="modal-box max-w-2xl">
    <PairWizard onclose={() => dialog.close()} ondone={goDevice} />
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
