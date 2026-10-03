<script lang="ts">
  // The `source` decides capabilities: its optional remove lights up delete. Call
  // sites mount a fresh instance per open, so per-open state resets by remount.
  import { onMount } from 'svelte';
  import { errMsg } from '../api/client';
  import { downloadFile, type AFCEntry, type FileSource } from '../api/files';
  import { fileFacts } from '../format';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import PreviewImage, { isPreviewableImage } from './PreviewImage.svelte';

  let {
    source,
    title,
    subtitle,
    rootLabel = 'Files',
    onclose,
  }: {
    source: FileSource;
    title: string;
    subtitle: string;
    rootLabel?: string;
    onclose: () => void;
  } = $props();

  let path = $state(''); // current directory, '' = root
  let entries = $state<AFCEntry[]>([]);
  let loading = $state(false);
  let error = $state<string | null>(null);
  let deleteError = $state<string | null>(null);
  let listCtrl: AbortController | null = null;

  // The pending delete is keyed by file name: the flat navigator lists one
  // directory, so names are unique within it.
  let confirmDelete = $state<string | null>(null);
  let deleting = $state<string | null>(null);

  let preview = $state<AFCEntry | null>(null);

  // The file whose download is being prepared (its stat is in flight).
  let saving = $state<string | null>(null);
  let saveError = $state<string | null>(null);
  const saveCtrl = new AbortController();

  // Load a whole directory; one AFC listing already comes back folders-first.
  async function navigate(dir: string): Promise<void> {
    listCtrl?.abort();
    const ctrl = new AbortController();
    listCtrl = ctrl;
    path = dir;
    confirmDelete = null;
    loading = true;
    error = null;
    entries = [];
    try {
      const next = await source.list(dir, ctrl.signal);
      if (!ctrl.signal.aborted) entries = next;
    } catch (err) {
      if (!ctrl.signal.aborted) error = errMsg(err, 'file_list_failed');
    } finally {
      if (!ctrl.signal.aborted) loading = false;
    }
  }

  onMount(() => {
    void navigate('');
    return () => {
      listCtrl?.abort();
      saveCtrl.abort();
    };
  });

  const segments = $derived(path === '' ? [] : path.split('/'));

  function child(entryName: string): string {
    return path === '' ? entryName : `${path}/${entryName}`;
  }

  async function removeFile(name: string): Promise<void> {
    const remove = source.remove;
    if (!remove) return;
    deleting = name;
    deleteError = null;
    try {
      await remove(child(name));
      void navigate(path);
    } catch (err) {
      deleteError = errMsg(err, 'file_delete_failed');
    } finally {
      deleting = null;
    }
  }

  async function save(name: string): Promise<void> {
    saving = name;
    saveError = null;
    try {
      await downloadFile(source, child(name), name, saveCtrl.signal);
    } catch (err) {
      if (!saveCtrl.signal.aborted) saveError = errMsg(err, 'download_failed');
    } finally {
      if (saving === name) saving = null;
    }
  }
</script>

<Modal {title} {subtitle} closable class="flex h-[85vh] max-w-2xl flex-col gap-3 overflow-hidden" {onclose}>
  <ErrorLine error={deleteError} size="xs" className="shrink-0" />
  <ErrorLine error={saveError} size="xs" className="shrink-0" />

  {#if preview}
    {@const p = preview}
    <div class="flex shrink-0 items-center gap-2">
      <button type="button" class="btn btn-ghost btn-sm shrink-0" onclick={() => (preview = null)}>
        <Icon name="arrowLeft" size={16} /> Back
      </button>
      <div class="min-w-0 flex-1">
        <p class="truncate text-sm font-medium">{p.name}</p>
        <p class="truncate text-xs text-base-content/50">{fileFacts(p)}</p>
      </div>
      <button
        type="button"
        class="btn btn-primary btn-sm shrink-0"
        disabled={saving === p.name}
        onclick={() => save(p.name)}
      >
        {#if saving === p.name}
          <span class="loading loading-spinner loading-xs"></span>
        {:else}
          <Icon name="download" size={14} />
        {/if}
        Save
      </button>
    </div>
    <div class="flex min-h-0 basis-48 grow shrink items-center justify-center overflow-auto rounded-box bg-base-200 p-2">
      <PreviewImage src={source.previewUrl(child(p.name))} alt={p.name}>
        {#snippet fallback()}
          <div class="p-6 text-center text-sm text-base-content/60">
            <Icon name="alert" size={24} class="mx-auto mb-2 opacity-50" />
            Couldn't render a preview. Use Save to download the original.
          </div>
        {/snippet}
      </PreviewImage>
    </div>
  {:else}
    <div class="breadcrumbs shrink-0 text-sm">
      <ul>
        <li><button type="button" class="link-hover link font-medium" onclick={() => navigate('')}>{rootLabel}</button></li>
        {#each segments as seg, i (i)}
          <li>
            <button type="button" class="link-hover link max-w-40 truncate" onclick={() => navigate(segments.slice(0, i + 1).join('/'))}>{seg}</button>
          </li>
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
        <ErrorLine {error} variant="alert" className="m-3" />
      {:else}
        <ul class="divide-y divide-base-300/60">
          {#if path !== ''}
            <li class="flex items-center">
              <button
                type="button"
                class="flex w-full items-center gap-3 px-4 py-2 text-left"
                title="Up one folder"
                onclick={() => navigate(segments.slice(0, -1).join('/'))}
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
                  onclick={() => navigate(child(entry.name))}
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
                  onclick={() => (preview = entry)}
                >
                  <Icon name="image" size={18} class="shrink-0 text-primary/80" />
                  <div class="min-w-0 flex-1">
                    <p class="truncate text-sm">{entry.name}</p>
                    <p class="truncate text-xs text-base-content/50">{fileFacts(entry)}</p>
                  </div>
                </button>
              {:else}
                <Icon name="file" size={18} class="shrink-0 text-base-content/40" />
                <div class="min-w-0 flex-1">
                  <p class="truncate text-sm">{entry.name}</p>
                  <p class="truncate text-xs text-base-content/50">{fileFacts(entry)}</p>
                </div>
              {/if}

              {#if entry.kind === 'directory'}
                <!-- folders navigate on click; no download/delete actions -->
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
                    disabled={saving === entry.name}
                    onclick={() => save(entry.name)}
                  >
                    {#if saving === entry.name}
                      <span class="loading loading-spinner loading-xs"></span>
                    {:else}
                      <Icon name="download" size={15} />
                    {/if}
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
</Modal>
