<script module lang="ts">
  // Mirrors nativeImageType + transcodeImageExt in internal/handler/media_preview.go;
  // HEIC/HEIF are transcoded server-side.
  const PREVIEW_IMAGE_EXT = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'heic', 'heif']);

  // Both mirror streamType in internal/handler/media_preview.go.
  const PLAYABLE_VIDEO_EXT = new Set(['mov', 'mp4', 'm4v']);
  const PLAYABLE_AUDIO_EXT = new Set(['m4a', 'mp3', 'aac', 'opus', 'ogg', 'wav']);

  const extOf = (name: string) => name.split('.').pop()?.toLowerCase() ?? '';

  export function isPreviewableImage(name: string): boolean {
    return PREVIEW_IMAGE_EXT.has(extOf(name));
  }

  export function isPlayableVideo(name: string): boolean {
    return PLAYABLE_VIDEO_EXT.has(extOf(name));
  }

  export function isPlayableAudio(name: string): boolean {
    return PLAYABLE_AUDIO_EXT.has(extOf(name));
  }
</script>

<script lang="ts">
  // Mount one per image (the lightbox keys it by path), so an image never
  // inherits another's load state.
  import type { Snippet } from 'svelte';

  let { src, alt, placeholder, fallback }: {
    src: string;
    alt: string;
    /** Shown while src loads: a thumbnail already at hand. */
    placeholder?: string;
    fallback: Snippet;
  } = $props();

  let loading = $state(true);
  let failed = $state(false);
</script>

{#if failed}
  {@render fallback()}
{:else}
  {#if loading && placeholder}
    <img class="h-full w-full rounded object-contain" src={placeholder} {alt} />
  {:else if loading}
    <span class="loading loading-spinner loading-md"></span>
  {/if}
  <img
    class="max-h-full max-w-full rounded object-contain"
    class:hidden={loading}
    {src}
    {alt}
    onload={() => (loading = false)}
    onerror={() => (failed = true)}
  />
{/if}
