<script lang="ts">
  import { wallpaperUrl, type Connection } from '../api/devices';
  import Icon from './Icon.svelte';

  let { udid, connection, lockScreen = false }: { udid: string; connection: Connection; lockScreen?: boolean } = $props();

  const screen = $derived(lockScreen ? 'lock' : 'home');
</script>

<div class="device-phone-preview" aria-hidden="true">
  <div class="mockup-phone shadow-sm">
    <div class="mockup-phone-camera"></div>
    <div class="mockup-phone-display grid place-items-center bg-black text-white/50">
      {#if connection === 'offline'}
        <Icon name="offline" size={120} stroke={1.5} />
      {:else}
        <!-- Keyed on the wallpaper-URL inputs so a failed load (onerror hides
             the img) resets when the device or screen changes. -->
        {#key `${udid}:${screen}`}
          <img
            src={wallpaperUrl(udid, screen)}
            alt=""
            onerror={(event) => ((event.currentTarget as HTMLImageElement).hidden = true)}
          />
        {/key}
      {/if}
    </div>
  </div>
</div>
