<script lang="ts">
  // Shown while the phone reports Find My on; the action it guards stays blocked.
  import { hardwareResources } from '../stores.svelte';
  import Alert from './Alert.svelte';

  let { udid }: { udid: string } = $props();

  const hardware = $derived(hardwareResources.for(udid));
</script>

<Alert tone="warning" title="Find My iPhone is on" class="mt-3 text-sm">
  <p>Turn it off on the phone, then check again</p>
  <p class="mt-1">Settings → [your name] → Find My</p>
  {#snippet actions()}
    <button type="button" class="btn btn-ghost btn-xs" disabled={hardware.loading} onclick={() => hardware.refresh()}>
      {#if hardware.loading}<span class="loading loading-spinner loading-xs"></span>{/if}
      Check again
    </button>
  {/snippet}
</Alert>
