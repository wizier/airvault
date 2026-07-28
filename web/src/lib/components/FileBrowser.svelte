<script lang="ts">
  // One AFC file manager for every source: app Documents (house_arrest) and the
  // device media partition (com.apple.afc). The `source` decides capabilities —
  // its optional remove lights up delete. Call sites mount a fresh instance per
  // open (inside {#if}), so per-open state resets by remount.
  import { untrack } from 'svelte';
  import { errRef } from '../api/client';
  import { PreviewTransfer, isPreviewableImage } from '../preview.svelte';
  import type { AFCEntry, FileSource } from '../api/files';
  import { Downloads } from '../download.svelte';
  import { errorRefText, type ErrorRef } from '../error-text';
  import { formatBytes, formatDate } from '../format';
  import { modalOpen } from '../modal';
  import DownloadProgress from './DownloadProgress.svelte';
  import Icon from './Icon.svelte';

  let {
    source,
    title,
    subtitle,
    rootLabel = 'Files',
    open = $bindable(false),
  }: {
    source: FileSource;
    title: string;
    subtitle: string;
    rootLabel?: string;
    open?: boolean;
  } = $props();

  let path = $state(''); // current directory, '' = root
  let entries = $state<AFCEntry[]>([]);
  let loading = $state(false);
  let failure = $state<ErrorRef | null>(null);
  const error = $derived(failure ? errorRefText(failure) : null);
  type Note = { text: string; tone: 'ok' } | { error: ErrorRef; tone: 'error' };
  let note = $state<Note | null>(null);
  const noteText = $derived(
    note?.tone === 'error' ? errorRefText(note.error) : note?.text,
  );
  let reloadNonce = $state(0);

  // Downloads (SSE progress + cancel) and the pending delete are keyed by file
  // name: the flat navigator lists one directory, so names are unique within it.
  const downloads = new Downloads((error) => (note = { error, tone: 'error' }));
  let confirmDelete = $state<string | null>(null);
  let deleting = $state<string | null>(null);

  // Inline preview (one file at a time). Images — including HEIC via a
  // server-side transcode — render inline; other types offer Save only.
  let preview = $state<AFCEntry | null>(null);
  const pv = new PreviewTransfer();

  function openPreview(entry: AFCEntry): void {
    pv.cancel();
    preview = entry;
    pv.begin();
  }
  function closePreview(): void {
    pv.cancel();
    preview = null;
  }

  async function loadDirectory(dir: string, signal: AbortSignal): Promise<void> {
    loading = true;
    failure = null;
    entries = [];
    try {
      const next = await untrack(() => source).list(dir, signal);
      if (!signal.aborted) entries = next;
    } catch (err) {
      if (!signal.aborted) failure = errRef(err, 'file_list_failed');
    } finally {
      if (!signal.aborted) loading = false;
    }
  }

  // (Re)load the whole directory when the modal opens, the path changes, or a
  // delete bumps reloadNonce. One AFC listing already comes back folders-first.
  $effect(() => {
    if (!open) return;
    const dir = path;
    void reloadNonce;
    confirmDelete = null;
    const ctrl = new AbortController();
    void loadDirectory(dir, ctrl.signal);
    return () => ctrl.abort();
  });

  // A transfer belongs to the visible logical directory. A metadata-only
  // reload (for example after deleting another file) must not cancel it.
  $effect(() => {
    if (!open) return;
    void path;
    return () => {
      downloads.cancelAll();
      pv.cancel();
      preview = null; // removing <img> aborts its HTTP request
    };
  });

  const segments = $derived(path === '' ? [] : path.split('/'));

  function child(entryName: string): string {
    return path === '' ? entryName : `${path}/${entryName}`;
  }
  function jump(depth: number): void {
    path = depth <= 0 ? '' : segments.slice(0, depth).join('/');
  }

  function downloadFile(entry: AFCEntry): void {
    note = null;
    void downloads.start(entry.name, (id) => source.downloadUrl(child(entry.name), id), entry.name);
  }
  function pct(entry: AFCEntry): number | null {
    return downloads.percent(entry.name);
  }

  async function removeFile(name: string): Promise<void> {
    const remove = source.remove;
    if (!remove) return;
    deleting = name;
    note = null;
    try {
      await remove(path === '' ? name : `${path}/${name}`);
      confirmDelete = null;
      reloadNonce += 1;
    } catch (err) {
      note = { error: errRef(err, 'file_delete_failed'), tone: 'error' };
    } finally {
      deleting = null;
    }
  }

</script>

<dialog class="modal" {@attach modalOpen(open)} onclose={() => (open = false)}>
  <div class="modal-box flex h-[85vh] max-w-2xl flex-col gap-3 overflow-hidden">
    <div class="flex shrink-0 items-start justify-between gap-3">
      <div class="min-w-0">
        <h3 class="truncate text-lg font-bold">{title}</h3>
        <p class="mt-0.5 text-sm text-base-content/60">{subtitle}</p>
      </div>
      <button type="button" class="btn btn-square btn-ghost btn-sm" aria-label="Close" onclick={() => (open = false)}>
        <Icon name="x" size={16} />
      </button>
    </div>

    {#if note}
      <p class={`flex shrink-0 items-center gap-1.5 text-xs ${note.tone === 'error' ? 'text-error' : 'text-success'}`}>
        <Icon name={note.tone === 'error' ? 'alert' : 'check'} size={13} stroke={2} />
        {noteText}
      </p>
    {/if}

    {#if preview}
      {@const p = preview}
      {@const requestId = pv.token}
      <div class="flex shrink-0 items-center gap-2">
        <button type="button" class="btn btn-ghost btn-sm shrink-0" onclick={closePreview}>
          <Icon name="arrowLeft" size={16} /> Back
        </button>
        <div class="min-w-0 flex-1">
          <p class="truncate text-sm font-medium">{p.name}</p>
          <p class="truncate text-xs text-base-content/50">
            {formatBytes(p.size ?? 0)}{p.modified ? ` · ${formatDate(p.modified)}` : ''}
          </p>
        </div>
        {#if downloads.active(p.name)}
          <DownloadProgress percent={pct(p)} oncancel={() => downloads.cancel(p.name)} />
        {:else}
          <button type="button" class="btn btn-primary btn-sm shrink-0" onclick={() => downloadFile(p)}>
            <Icon name="download" size={14} /> Save
          </button>
        {/if}
      </div>
      <div class="flex min-h-0 basis-48 grow shrink items-center justify-center overflow-auto rounded-box bg-base-200 p-2">
        {#if pv.error}
          <div class="p-6 text-center text-sm text-base-content/60">
            <Icon name="alert" size={24} class="mx-auto mb-2 opacity-50" />
            Couldn't render a preview. Use Save to download the original.
          </div>
        {:else}
          {#if pv.loading}
            <span class="loading loading-spinner loading-md"></span>
          {/if}
          <img
            class="max-h-full max-w-full rounded object-contain"
            class:hidden={pv.loading}
            src={source.previewUrl(child(p.name))}
            alt={p.name}
            onload={() => pv.settled(requestId, false)}
            onerror={() => pv.settled(requestId, true)}
          />
        {/if}
      </div>
    {:else}
      <div class="breadcrumbs shrink-0 text-sm">
        <ul>
          <li><button type="button" class="link-hover link font-medium" onclick={() => jump(0)}>{rootLabel}</button></li>
          {#each segments as seg, i (i)}
            <li><button type="button" class="link-hover link max-w-40 truncate" onclick={() => jump(i + 1)}>{seg}</button></li>
          {/each}
        </ul>
      </div>

      <div class="min-h-0 basis-48 grow shrink overflow-auto rounded-box bg-base-200">
        {#if loading}
          <p class="flex items-center gap-2 p-4 text-sm text-base-content/60">
            <span class="loading loading-spinner loading-sm"></span>
            Asking the phone…
          </p>
        {:else if error}
          <div role="alert" class="alert alert-error alert-soft m-3">
            <Icon name="alert" size={16} />
            <span class="text-sm">{error}</span>
          </div>
        {:else}
          <ul class="divide-y divide-base-300/60">
            {#if path !== ''}
              <li class="flex items-center">
                <button
                  type="button"
                  class="flex w-full items-center gap-3 px-4 py-2 text-left"
                  title="Up one folder"
                  onclick={() => jump(segments.length - 1)}
                >
                  <Icon name="levelUp" size={18} class="shrink-0 text-base-content/60" />
                  <span class="text-sm font-medium text-base-content/70">..</span>
                </button>
              </li>
            {/if}
            {#each entries as entry (entry.name)}
              <li class="flex items-center gap-3 px-4 py-2">
                {#if entry.kind === 'directory'}
                  <button
                    type="button"
                    class="flex min-w-0 flex-1 items-center gap-3 text-left"
                    onclick={() => (path = child(entry.name))}
                  >
                    <Icon name="folder" size={18} class="shrink-0 text-base-content/60" />
                    <span class="min-w-0 flex-1 truncate text-sm font-medium">{entry.name}</span>
                    <Icon name="arrowRight" size={14} class="shrink-0 text-base-content/40" />
                  </button>
                {:else if isPreviewableImage(entry.name)}
                  <button
                    type="button"
                    class="flex min-w-0 flex-1 items-center gap-3 text-left"
                    title="Preview"
                    onclick={() => openPreview(entry)}
                  >
                    <Icon name="image" size={18} class="shrink-0 text-primary/80" />
                    <div class="min-w-0 flex-1">
                      <p class="truncate text-sm">{entry.name}</p>
                      <p class="truncate text-xs text-base-content/50">
                        {formatBytes(entry.size ?? 0)}{entry.modified ? ` · ${formatDate(entry.modified)}` : ''}
                      </p>
                    </div>
                  </button>
                {:else}
                  <Icon name="file" size={18} class="shrink-0 text-base-content/40" />
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm">{entry.name}</p>
                    <p class="truncate text-xs text-base-content/50">
                      {formatBytes(entry.size ?? 0)}{entry.modified ? ` · ${formatDate(entry.modified)}` : ''}
                    </p>
                  </div>
                {/if}

                {#if entry.kind === 'directory'}
                  <!-- folders navigate on click; no download/delete actions -->
                {:else if downloads.active(entry.name)}
                  <DownloadProgress percent={pct(entry)} oncancel={() => downloads.cancel(entry.name)} />
                {:else if confirmDelete === entry.name}
                  <div class="flex shrink-0 items-center gap-1">
                    <button
                      type="button"
                      class="btn btn-error btn-xs"
                      disabled={deleting === entry.name}
                      onclick={() => removeFile(entry.name)}
                    >
                      {#if deleting === entry.name}
                        <span class="loading loading-spinner loading-xs"></span>
                      {:else}
                        Delete
                      {/if}
                    </button>
                    <button type="button" class="btn btn-ghost btn-xs" onclick={() => (confirmDelete = null)}>
                      Cancel
                    </button>
                  </div>
                {:else}
                  <div class="flex shrink-0 items-center gap-1">
                    <button
                      type="button"
                      class="btn btn-square btn-ghost btn-xs"
                      title="Download"
                      aria-label={`Download ${entry.name}`}
                      onclick={() => downloadFile(entry)}
                    >
                      <Icon name="download" size={15} />
                    </button>
                    {#if source.remove}
                      <button
                        type="button"
                        class="btn btn-square btn-ghost btn-xs"
                        title="Delete"
                        aria-label={`Delete ${entry.name}`}
                        onclick={() => (confirmDelete = entry.name)}
                      >
                        <Icon name="trash" size={15} />
                      </button>
                    {/if}
                  </div>
                {/if}
              </li>
            {/each}
            {#if entries.length === 0}
              <li class="px-4 py-3 text-sm text-base-content/50">This folder is empty.</li>
            {/if}
          </ul>
        {/if}
      </div>
    {/if}
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
