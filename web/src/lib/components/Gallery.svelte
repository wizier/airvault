<script lang="ts">
  // Backed by iOS's own thumbnails (from the phone or a backup), so it stays
  // cheap for tens of thousands of photos: tiles fetch thumbs near the viewport,
  // far-off thumbs are evicted, and the roll pages in on scroll.
  import { onMount } from 'svelte';
  import { ApiError, errMsg } from '../api/client';
  import type { GalleryAsset, GalleryFilter, GalleryMonth, GallerySource } from '../api/gallery';
  import { downloadFile, type FileStat } from '../api/files';
  import { createBatchLoader, nearViewport } from '../batch-loader.svelte';
  import { fileFacts, formatDateTime } from '../format';
  import { pullNear } from '../pull-near';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import PreviewImage, { isPlayableVideo, isPreviewableImage } from './PreviewImage.svelte';

  let {
    source,
    title,
    subtitle = 'Camera roll',
    rescan = true,
    onclose,
  }: {
    source: GallerySource;
    title: string;
    subtitle?: string;
    /** A phone's roll changes under the gallery; a backup's never does. */
    rescan?: boolean;
    onclose: () => void;
  } = $props();

  let filter = $state('');
  let month = $state(''); // "2026-10"; '' is every date
  let months = $state<GalleryMonth[]>([]);
  let filters = $state<GalleryFilter[]>([]);
  const chips = $derived(filters.filter((f) => !f.group));
  const chipGroup = $props.id();
  const pickerGroups = $derived.by(() => {
    const groups = new Map<string, GalleryFilter[]>();
    for (const f of filters) if (f.group) groups.set(f.group, [...(groups.get(f.group) ?? []), f]);
    return [...groups];
  });

  const PAGE = 120;
  // Full-res image previews and Save downloads.
  const media = $derived(source.files);
  let assets = $state<GalleryAsset[]>([]);
  let total = $state(0);
  let revision = $state('');
  let loading = $state(false);
  let loadingMore = $state(false);
  let error = $state<string | null>(null);
  let moreError = $state<string | null>(null);
  let pageCtrl: AbortController | null = null;

  // JPEG data URLs keyed by path; MAX_THUMBS bounds how many stay alive.
  let thumbs = $state<Record<string, string>>({});
  const MAX_THUMBS = 600; // 5 × PAGE
  const near = new Set<string>(); // paths inside the tile observer's margin
  const thumbLoader = createBatchLoader<string>({
    batchSize: 30,
    debounceMs: 120,
    fetchBatch: (paths, signal) => source.thumbs(paths, signal),
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
  let videoFailed = $state(false);
  let livePlaying = $state(false);
  let lbStat = $state<Promise<FileStat>>();
  let saving = $state(false);
  let saveError = $state<string | null>(null);
  const saveCtrl = new AbortController();

  const lbIndex = $derived(lightbox ? assets.findIndex((a) => a.path === lightbox!.path) : -1);

  function canPreview(a: GalleryAsset): boolean {
    return a.kind === 'photo' && !a.missing && isPreviewableImage(a.name);
  }

  function canPlay(a: GalleryAsset): boolean {
    return a.kind === 'video' && !a.missing && isPlayableVideo(a.name);
  }

  // taken is on the clock where the photo was taken, as months are counted.
  function monthOf(a: GalleryAsset | undefined): string {
    return a?.taken?.slice(0, 7) ?? '';
  }

  function monthLabel(key: string): string {
    const [year, mon] = key.split('-').map(Number);
    return new Date(year, mon - 1, 1).toLocaleDateString(undefined, { month: 'long', year: 'numeric' });
  }

  async function loadMonths(): Promise<void> {
    months = [];
    if (!source.months) return;
    const asked = filter;
    const list = await source.months(asked).catch(() => []); // the picker just stays hidden
    if (asked === filter) months = list;
  }

  function setFilter(key: string): void {
    filter = key;
    month = '';
    void load();
    void loadMonths();
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
      const page = await source.page({
        filter,
        month,
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
      const page = await source.page({
        filter,
        month,
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
    lbStat = a.missing ? undefined : media.stat(a.path);
    saveError = null;
    videoFailed = false;
    livePlaying = false;
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
    return a.liveVideo ? 'Live Photo' : 'Photo';
  }
  function assetFormat(a: GalleryAsset): string {
    return a.name.includes('.') ? (a.name.split('.').pop() ?? '').toUpperCase() : '';
  }

  // The parent mounts a fresh Gallery for every open, so the gallery session
  // belongs to this component instance rather than to a reactive state effect.
  onMount(() => {
    void load();
    void loadMonths();
    void source.filters?.().then((list) => (filters = list), () => {}); // the filters just stay hidden
    return () => {
      pageCtrl?.abort();
      pageCtrl = null;
      saveCtrl.abort();
      resetThumbs();
    };
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

<Modal
  {title}
  subtitle={total ? `${total.toLocaleString()} items` : subtitle}
  closable
  size="wide"
  class="relative gap-3"
  {onclose}
>
  {#snippet headerActions()}
    {#if rescan}
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
    {/if}
  {/snippet}

  {#if filters.length > 0 || months.length > 0}
    <div class="flex shrink-0 flex-wrap items-center gap-1">
      {#each chips as f (f.key)}
        <input
          type="radio"
          class="btn btn-xs"
          name={chipGroup}
          aria-label={`${f.label} ${f.count.toLocaleString()}`}
          checked={filter === f.key}
          onchange={() => setFilter(f.key)}
        />
      {/each}
      {#if pickerGroups.length > 0}
        <select
          class="select select-xs ml-auto w-auto"
          aria-label="Album"
          value={filters.some((f) => f.group && f.key === filter) ? filter : ''}
          onchange={(e) => setFilter(e.currentTarget.value)}
        >
          <option value="">Albums…</option>
          {#each pickerGroups as [group, list] (group)}
            <optgroup label={group}>
              {#each list as f (f.key)}
                <option value={f.key}>{f.label} · {f.count}</option>
              {/each}
            </optgroup>
          {/each}
        </select>
      {/if}
      {#if months.length > 0}
        <select
          class={`select select-xs w-auto ${pickerGroups.length > 0 ? '' : 'ml-auto'}`}
          aria-label="Month"
          bind:value={month}
          onchange={() => load()}
        >
          <option value="">All dates</option>
          {#each months as m (m.month)}
            <option value={m.month}>{monthLabel(m.month)} · {m.count}</option>
          {/each}
        </select>
      {/if}
    </div>
  {/if}

  <div class="min-h-0 flex-1 overflow-auto rounded-box bg-base-200 p-1" {@attach tiles.root}>
    {#if loading}
      <p class="flex items-center gap-2 p-4 text-sm text-base-content/60">
        <span class="loading loading-spinner loading-sm"></span>
        Reading the camera roll…
      </p>
    {:else if error}
      <ErrorLine {error} variant="alert" className="m-3" />
    {:else if assets.length === 0}
      <p class="p-4 text-sm text-base-content/50">Nothing here</p>
    {:else}
      <div class="grid grid-cols-3 gap-1 sm:grid-cols-4 md:grid-cols-6">
        {#each assets as a, i (a.path)}
          {#if monthOf(a) && monthOf(a) !== monthOf(assets[i - 1])}
            <h3 class="col-span-full px-1 pb-1 pt-3 text-sm font-semibold first:pt-1">{monthLabel(monthOf(a))}</h3>
          {/if}
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
            {#if a.liveVideo}
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
            {#if a.missing}
              <span
                class="absolute bottom-1 left-1 rounded-full bg-black/55 p-0.5 text-white"
                title="The original is only in iCloud"
              >
                <Icon name="cloud" size={12} />
              </span>
            {/if}
          </button>
        {/each}
      </div>
      <!-- The next page pulls in as the end of the grid nears the view. -->
      {#if !loadingMore && !moreError && assets.length < total}
        <div {@attach pullNear(loadMore, '600px')} class="h-px"></div>
      {/if}
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
              {#if a.missing}
                The original is only in iCloud; this backup keeps just the thumbnail.
              {:else if a.kind === 'video'}
                This video can't play in the browser — Save it to watch.
              {:else}
                Couldn't render a preview — Save the original.
              {/if}
            </p>
          </div>
        {/snippet}
        {#if canPlay(a) && !videoFailed}
          {#key a.path}
            <!-- svelte-ignore a11y_media_has_caption -->
            <video
              src={media.previewUrl(a.path)}
              class="max-h-full max-w-full rounded"
              controls
              autoplay
              playsinline
              onerror={() => (videoFailed = true)}
            ></video>
          {/key}
        {:else if livePlaying && a.liveVideo}
          <!-- svelte-ignore a11y_media_has_caption -->
          <video
            src={media.previewUrl(a.liveVideo)}
            class="max-h-full max-w-full rounded"
            autoplay
            playsinline
            onended={() => (livePlaying = false)}
            onerror={() => (livePlaying = false)}
          ></video>
        {:else if canPreview(a)}
          {#key a.path}
            <PreviewImage src={media.previewUrl(a.path)} alt={a.name} placeholder={thumbs[a.path]} fallback={noPreview} />
          {/key}
        {:else}
          {@render noPreview()}
        {/if}
        {#if a.liveVideo && canPreview(a) && !livePlaying}
          <button
            type="button"
            class="btn btn-xs absolute left-2 top-2 z-10 border-none bg-black/55 text-white"
            onclick={() => (livePlaying = true)}
          >
            <Icon name="play" size={12} /> LIVE
          </button>
        {/if}
      </div>

      <div class="flex shrink-0 items-end justify-between gap-3">
        <div class="min-w-0">
          <p class="truncate text-sm font-medium">{a.name}</p>
          <p class="truncate text-xs text-base-content/60">
            {assetType(a)}{assetFormat(a) ? ` · ${assetFormat(a)}` : ''}{a.taken
              ? ` · Taken ${formatDateTime(a.taken)}`
              : ''}
          </p>
          <!-- A failed stat leaves size and date out (the empty catch handles it); a
               photo kept only in iCloud has none, and {#await undefined} would render at once. -->
          {#if lbStat}
            {#await lbStat then stat}
              <p class="truncate text-xs text-base-content/50">{fileFacts(stat)}</p>
            {:catch}{/await}
          {/if}
          <ErrorLine error={saveError} size="xs" />
        </div>
        <button
          type="button"
          class="btn btn-primary btn-sm shrink-0"
          disabled={saving || a.missing}
          onclick={() => save(a)}
        >
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
</Modal>
