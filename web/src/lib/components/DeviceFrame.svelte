<script lang="ts">
  import { wallpaperUrl, type Connection } from '../api/devices';
  import { deviceScreen } from '../device-ui';
  import Icon from './Icon.svelte';

  let {
    udid,
    productType,
    connection,
    lockScreen = false,
  }: {
    udid: string;
    productType?: string;
    connection: Connection;
    lockScreen?: boolean;
  } = $props();

  const screen = $derived(deviceScreen(productType));
  const wallpaper = $derived(lockScreen ? 'lock' : 'home');
</script>

<div class="device-frame" data-form={screen.form} style:--frame-ratio={screen.ratio} aria-hidden="true">
  {#if screen.form === 'phone'}
    <div class="device-frame-island"></div>
  {/if}
  <div class="device-frame-screen grid place-items-center bg-black text-white/50">
    {#if connection === 'offline'}
      <Icon name="offline" size={22} stroke={1.5} />
    {:else}
      <!-- Keyed on the wallpaper-URL inputs so a failed load (onerror hides
           the img) resets when the device or screen changes. -->
      {#key `${udid}:${wallpaper}`}
        <img
          src={wallpaperUrl(udid, wallpaper)}
          alt=""
          onerror={(event) => ((event.currentTarget as HTMLImageElement).hidden = true)}
        />
      {/key}
    {/if}
  </div>
</div>
