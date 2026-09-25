<script lang="ts">
  import { type Device } from '../api/devices';
  import { batteryUi } from '../device-ui';
  import { batteryResources } from '../stores.svelte';
  import Icon from './Icon.svelte';

  let { device, badge = false }: { device: Device; badge?: boolean } = $props();
  const res = $derived(batteryResources.for(device.udid));
  const battery = $derived(batteryUi(res.data));
  // A boolean, so a device list refresh (a new device object) never restarts
  // the poll or its immediate battery read.
  const online = $derived(device.connection !== 'offline');

  // Offline devices are not polled — presence comes from SSE, whose connection
  // change re-runs this effect.
  $effect(() => {
    if (!online) return;
    const stop = res.start();
    const timer = setInterval(() => {
      if (!document.hidden) void res.refresh();
    }, 30_000);
    return () => {
      clearInterval(timer);
      stop();
    };
  });
</script>

{#if battery && online}
  <span
    class={`${badge ? 'badge badge-sm badge-ghost' : 'flex'} items-center gap-1 whitespace-nowrap font-medium tabular-nums ${battery.cls}`}
    title={battery.title}
  >
    <Icon name={battery.icon} size={15} stroke={2} />
    {battery.label}
  </span>
{/if}
