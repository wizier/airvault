<script lang="ts">
  // Shown while the phone reports Find My on; the action it guards stays blocked.
  import { hardwareResources } from '../stores.svelte';
  import Icon from './Icon.svelte';

  let { udid }: { udid: string } = $props();

  const hardware = $derived(hardwareResources.for(udid));
</script>

<div role="alert" class="alert alert-warning alert-soft mt-3">
  <Icon name="alert" size={18} />
  <div class="text-sm">
    <p class="font-medium">Find My iPhone is on</p>
    <p class="mt-1 opacity-80">Turn it off on the phone, then check again</p>
    <p class="mt-1 opacity-80">Settings → [your name] → Find My</p>
  </div>
  <button type="button" class="btn btn-ghost btn-xs" disabled={hardware.loading} onclick={() => hardware.refresh()}>
    {#if hardware.loading}<span class="loading loading-spinner loading-xs"></span>{/if}
    Check again
  </button>
</div>
