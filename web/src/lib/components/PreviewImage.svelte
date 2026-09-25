<script module lang="ts">
  // Extensions the server can render inline as an <img>; HEIC/HEIF go through a
  // server-side transcode. Mirrors internal/handler/media_preview.go (nativeImageType + transcodeImageExt).
  const PREVIEW_IMAGE_EXT = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'heic', 'heif']);

  export function isPreviewableImage(name: string): boolean {
    const ext = name.split('.').pop()?.toLowerCase() ?? '';
    return PREVIEW_IMAGE_EXT.has(ext);
  }
</script>

<script lang="ts">
  // One inline full-size preview, shared by the file browser and the gallery
  // lightbox. Mount one per image (the lightbox keys it by path), so an image
  // never inherits another's load state.
  import type { Snippet } from 'svelte';

  let { src, alt, fallback }: { src: string; alt: string; fallback: Snippet } = $props();

  let loading = $state(true);
  let failed = $state(false);
</script>

{#if failed}
  {@render fallback()}
{:else}
  {#if loading}
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
