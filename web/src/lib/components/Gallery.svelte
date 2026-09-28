<script lang="ts">
  // Backed by the phone's own thumbnails, so it stays cheap for tens of thousands
  // of photos: tiles fetch thumbs near the viewport, far-off thumbs are evicted,
  // and the roll pages in on scroll.
  import { onMount } from 'svelte';
  import { ApiError, errMsg } from '../api/client';
  import { galleryPage, mediaThumbsBatch, type GalleryAsset } from '../api/gallery';
  import { deviceFileSource, downloadFile, type FileStat } from '../api/files';
  import { createBatchLoader, nearViewport } from '../batch-loader.svelte';
  import { fileFacts } from '../format';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import PreviewImage, { isPreviewableImage } from './PreviewImage.svelte';

  let { udid, name, onclose }: { udid: string; name: string; onclose: () => void } = $props();

  let dialog: HTMLDialogElement;
  const PAGE = 120;
  // Full-res image previews and Save downloads ride the media-partition FileSource.
  const media = $derived(deviceFileSource(udid));
  let assets = $state<GalleryAsset[]>([]);
  let total = $state(0);
  let revision = $state('');
  let loading = $state(false);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);
  let moreError = $state<string | null>(null);
  let sentinel = $state<HTMLElement | null>(null);
  let scroller = $state<HTMLElement | null>(null);
  let pageCtrl: AbortController | null = null;

  // JPEG data URLs keyed by path; MAX_THUMBS bounds how many stay alive.
  let thumbs = $state<Record<string, string>>({});
  const MAX_THUMBS = 600; // 5 × PAGE
  const near = new Set<string>(); // paths inside the tile observer's margin
  const thumbLoader = createBatchLoader<string>({
    batchSize: 30,
    debounceMs: 120,
    fetchBatch: (paths, signal) => mediaThumbsBatch(udid, paths, signal),
    onBatch: (_paths, urls) => {
      for (const [path, url] of Object.entries(urls)) {
        if (near.has(path)) thumbs[path] = url;
      }
      trimThumbs();
    },
  });

  const tiles = nearViewport('1200px 0px', (path, isNear) => {
    if (isNear) {
      near.add(path);
      if (thumbs[path] === undefined) thumbLoader.queue(path);
    } else {
      near.delete(path);
    }
  });

  // Evict thumbs of tiles far off-screen, oldest-fetched first.
  function trimThumbs(): void {
    const paths = Object.keys(thumbs);
    let excess = paths.length - MAX_THUMBS;
    for (const path of paths) {
      if (excess <= 0) return;
      if (near.has(path)) continue;
      delete thumbs[path];
      excess--;
    }
  }

  function resetThumbs(): void {
    thumbLoader.reset();
    thumbs = {};
  }

  let lightbox = $state<GalleryAsset | null>(null);
  let lbStat = $state<Promise<FileStat>>();
  let saving = $state(false);
  let saveError = $state<string | null>(null);
  const saveCtrl = new AbortController();

  const lbIndex = $derived(lightbox ? assets.findIndex((a) => a.path === lightbox!.path) : -1);

  function canPreview(a: GalleryAsset): boolean {
    return a.kind === 'photo' && isPreviewableImage(a.name);
  }

  async function load(): Promise<void> {
    pageCtrl?.abort();
    const ctrl = new AbortController();
    pageCtrl = ctrl;
    loading = true;
    loadingMore = false;
    error = null;
    moreError = null;
    assets = [];
    resetThumbs();
    total = 0;
    revision = '';
    try {
      const page = await galleryPage(udid, {
        offset: 0,
        limit: PAGE,
        signal: ctrl.signal,
      });
      if (ctrl.signal.aborted) return;
      assets = page.assets;
      total = page.total;
      revision = page.revision;
    } catch (err) {
      if (!ctrl.signal.aborted) error = errMsg(err, 'gallery_failed');
    } finally {
      if (pageCtrl === ctrl) {
        pageCtrl = null;
        loading = false;
      }
    }
  }

  async function loadMore(): Promise<void> {
    if (loadingMore || loading || assets.length >= total) return;
    const ctrl = new AbortController();
    pageCtrl = ctrl;
    loadingMore = true;
    moreError = null;
    try {
      const page = await galleryPage(udid, {
        offset: assets.length,
        limit: PAGE,
        revision,
        signal: ctrl.signal,
      });
      if (ctrl.signal.aborted) return;
      assets = [...assets, ...page.assets];
      total = page.total;
    } catch (err) {
      if (ctrl.signal.aborted) return;
      if (err instanceof ApiError && err.code === 'gallery_revision_changed') void load();
      // No automatic retry: only Retry pulls again.
      else moreError = errMsg(err, 'gallery_failed');
    } finally {
      if (pageCtrl === ctrl) {
        pageCtrl = null;
        loadingMore = false;
      }
    }
  }

  function openLightbox(a: GalleryAsset): void {
    lightbox = a;
    // Size + date come from a single on-demand stat; {#await} ignores the
    // answer for a photo the user has already stepped past.
    lbStat = media.stat(a.path);
    saveError = null;
  }

  // Step to the previous/next asset in the roll (prefetch a page near the end).
  function step(delta: number): void {
    if (lbIndex < 0) return;
    const i = lbIndex + delta;
    if (i < 0 || i >= assets.length) return;
    openLightbox(assets[i]);
    if (i >= assets.length - 12) void loadMore();
  }

  async function save(a: GalleryAsset): Promise<void> {
    saving = true;
    saveError = null;
    try {
      await downloadFile(media, a.path, a.name, saveCtrl.signal);
    } catch (err) {
      if (!saveCtrl.signal.aborted) saveError = errMsg(err, 'download_failed');
    } finally {
      saving = false;
    }
  }

  function assetType(a: GalleryAsset): string {
    if (a.kind === 'video') return 'Video';
    return a.live ? 'Live Photo' : 'Photo';
  }
  function assetFormat(a: GalleryAsset): string {
    return a.name.includes('.') ? (a.name.split('.').pop() ?? '').toUpperCase() : '';
  }

  // The parent mounts a fresh Gallery for every open, so the gallery session
  // belongs to this component instance rather than to a reactive state effect.
  onMount(() => {
    void load();
    return () => {
      pageCtrl?.abort();
      pageCtrl = null;
      saveCtrl.abort();
      resetThumbs();
    };
  });

  // Infinite scroll: pull the next page when the sentinel nears the grid's
  // visible box. A fresh observer per page reports the sentinel's current
  // state, so a short page that leaves it in view pulls the next one too.
  $effect(() => {
    if (!sentinel || !scroller || loadingMore || moreError || assets.length >= total) return;
    const io = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) void loadMore();
      },
      { root: scroller, rootMargin: '600px' },
    );
    io.observe(sentinel);
    return () => io.disconnect();
  });

  function hideBroken(e: Event): void {
    (e.currentTarget as HTMLImageElement).style.visibility = 'hidden';
  }
</script>

<svelte:window
  onkeydown={(e) => {
    if (!lightbox) return;
    if (e.key === 'ArrowLeft') step(-1);
    else if (e.key === 'ArrowRight') step(1);
  }}
/>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} {onclose}>
  <div class="modal-box relative flex h-[90vh] max-h-[90vh] w-full max-w-5xl flex-col gap-3">
    <div class="flex shrink-0 items-start justify-between gap-3">
      <div class="min-w-0">
        <h3 class="truncate text-lg font-bold">Media — {name}</h3>
        <p class="mt-0.5 text-sm text-base-content/60">
          {total ? `${total.toLocaleString()} items` : 'Camera roll'}
        </p>
      </div>
      <div class="flex shrink-0 items-center gap-1">
        <button
          type="button"
          class="btn btn-square btn-ghost btn-sm"
          title="Rescan"
          aria-label="Rescan"
          disabled={loading}
          onclick={() => load()}
        >
          <Icon name="refresh" size={16} />
        </button>
        <button type="button" class="btn btn-square btn-ghost btn-sm" aria-label="Close" onclick={() => dialog.close()}>
          <Icon name="x" size={16} />
        </button>
      </div>
    </div>

    <div class="min-h-0 flex-1 overflow-auto rounded-box bg-base-200 p-1" bind:this={scroller} {@attach tiles.root}>
      {#if loading}
        <p class="flex items-center gap-2 p-4 text-sm text-base-content/60">
          <span class="loading loading-spinner loading-sm"></span>
          Reading the camera roll…
        </p>
      {:else if error}
        <ErrorLine {error} variant="alert" className="m-3" />
      {:else if assets.length === 0}
        <p class="p-4 text-sm text-base-content/50">Nothing here.</p>
      {:else}
        <div class="grid grid-cols-3 gap-1 sm:grid-cols-4 md:grid-cols-6">
          {#each assets as a (a.path)}
            <button
              type="button"
              class="group relative aspect-square overflow-hidden rounded bg-base-300"
              title={a.name}
              onclick={() => openLightbox(a)}
              {@attach tiles.item(a.path)}
            >
              <!-- Placeholder shows until this tile's batch fills thumbs[path]. -->
              <Icon name="image" size={22} class="absolute inset-0 m-auto text-base-content/20" />
              {#if thumbs[a.path]}
                <img
                  src={thumbs[a.path]}
                  alt={a.name}
                  class="absolute inset-0 h-full w-full object-cover transition group-hover:opacity-90"
                  onerror={hideBroken}
                />
              {/if}
              {#if a.live}
                <span
                  class="absolute left-1 top-1 rounded bg-black/55 px-1 text-[10px] font-semibold leading-tight text-white"
                >
                  LIVE
                </span>
              {/if}
              {#if a.kind === 'video'}
                <span class="absolute bottom-1 right-1 rounded-full bg-black/55 p-0.5 text-white">
                  <Icon name="play" size={12} />
                </span>
              {/if}
            </button>
          {/each}
        </div>
        <div bind:this={sentinel} class="h-px"></div>
        {#if loadingMore}
          <p class="flex items-center justify-center gap-2 p-3 text-sm text-base-content/60">
            <span class="loading loading-spinner loading-sm"></span>
          </p>
        {:else if moreError}
          <p class="flex items-center justify-center gap-2 p-3 text-sm text-error">
            <Icon name="alert" size={14} stroke={2} />
            {moreError}
            <button type="button" class="btn btn-ghost btn-xs" onclick={() => loadMore()}>Retry</button>
          </p>
        {/if}
      {/if}
    </div>

    {#if lightbox}
      {@const a = lightbox}
      <div class="absolute inset-0 z-10 flex flex-col gap-2 rounded-2xl bg-base-100 p-3">
        <div class="flex shrink-0 items-center justify-between gap-2">
          <button type="button" class="btn btn-ghost btn-sm" onclick={() => (lightbox = null)}>
            <Icon name="arrowLeft" size={16} /> Back
          </button>
          <button
            type="button"
            class="btn btn-square btn-ghost btn-sm"
            aria-label="Close"
            onclick={() => (lightbox = null)}
          >
            <Icon name="x" size={16} />
          </button>
        </div>

        <div class="relative flex min-h-0 flex-1 items-center justify-center overflow-auto">
          {#if lbIndex > 0}
            <button
              type="button"
              class="btn btn-circle btn-sm absolute left-1 top-1/2 z-10 -translate-y-1/2 border-none bg-base-100/70"
              aria-label="Previous"
              onclick={() => step(-1)}
            >
              <Icon name="arrowLeft" size={18} />
            </button>
          {/if}
          {#if lbIndex >= 0 && lbIndex < assets.length - 1}
            <button
              type="button"
              class="btn btn-circle btn-sm absolute right-1 top-1/2 z-10 -translate-y-1/2 border-none bg-base-100/70"
              aria-label="Next"
              onclick={() => step(1)}
            >
              <Icon name="arrowRight" size={18} />
            </button>
          {/if}
          {#snippet noPreview()}
            <div class="flex flex-col items-center gap-3 text-center">
              {#if thumbs[a.path]}
                <img src={thumbs[a.path]} alt={a.name} class="max-h-[55vh] max-w-full rounded" onerror={hideBroken} />
              {/if}
              <p class="text-sm text-base-content/60">
                {a.kind === 'video' ? 'Video — Save to view it.' : "Couldn't render a preview — Save the original."}
              </p>
            </div>
          {/snippet}
          {#if canPreview(a)}
            {#key a.path}
              <PreviewImage src={media.previewUrl(a.path)} alt={a.name} fallback={noPreview} />
            {/key}
          {:else}
            {@render noPreview()}
          {/if}
        </div>

        <div class="flex shrink-0 items-end justify-between gap-3">
          <div class="min-w-0">
            <p class="truncate text-sm font-medium">{a.name}</p>
            <p class="truncate text-xs text-base-content/60">
              {assetType(a)}{assetFormat(a) ? ` · ${assetFormat(a)}` : ''}
            </p>
            <!-- A failed stat leaves size and date out; the empty catch keeps it handled. -->
            {#await lbStat then stat}
              <p class="truncate text-xs text-base-content/50">{fileFacts(stat!)}</p>
            {:catch}{/await}
            <ErrorLine error={saveError} size="xs" />
          </div>
          <button type="button" class="btn btn-primary btn-sm shrink-0" disabled={saving} onclick={() => save(a)}>
            {#if saving}
              <span class="loading loading-spinner loading-xs"></span>
            {:else}
              <Icon name="download" size={14} />
            {/if}
            Save
          </button>
        </div>
      </div>
    {/if}
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
