<script lang="ts">
  // Modal host for the pairing wizard. The wizard is mounted only while the
  // modal is open, so its one-shot pair-state fetch and SSE reactions are scoped
  // to the visible flow.
  import { push } from 'svelte-spa-router';
  import { modalOpen } from '../modal';
  import PairWizard from './PairWizard.svelte';

  let { open = $bindable(false) }: { open?: boolean } = $props();

  function goDevice(udid: string) {
    open = false;
    push(`/device/${encodeURIComponent(udid)}`);
  }
</script>

<dialog class="modal" {@attach modalOpen(open)} onclose={() => (open = false)}>
  <div class="modal-box max-w-2xl">
    {#if open}
      <PairWizard onclose={() => (open = false)} ondone={goDevice} />
    {/if}
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
