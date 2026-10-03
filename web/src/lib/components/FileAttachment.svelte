<script lang="ts">
  // A file a message or a note carries: shown in place when the browser can,
  // saved on a click otherwise.
  import { onMount } from 'svelte';
  import type { BackupAttachment } from '../api/backup-contents';
  import { errMsg } from '../api/client';
  import { downloadFile, type FileSource } from '../api/files';
  import { formatBytes } from '../format';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import { isPlayableAudio, isPlayableVideo, isPreviewableImage } from './PreviewImage.svelte';

  let { file, files }: {
    file: BackupAttachment;
    files: FileSource;
  } = $props();

  let error = $state<string | null>(null);
  const ctrl = new AbortController();
  onMount(() => () => ctrl.abort());

  async function save(path: string): Promise<void> {
    error = null;
    try {
      await downloadFile(files, path, file.name, ctrl.signal);
    } catch (err) {
      if (!ctrl.signal.aborted) error = errMsg(err, 'download_failed');
    }
  }
</script>

{#if file.missing || !file.path}
  {#if /^https?:\/\//.test(file.name)}
    <a href={file.name} target="_blank" rel="noopener noreferrer" class="link wrap-anywhere">{file.name}</a>
  {:else}
    <span class="flex items-center gap-1 text-xs opacity-70" title={file.missing ? 'Not in the backup' : undefined}>
      {#if file.missing}<Icon name="cloud" size={12} />{/if}
      {file.name}
    </span>
  {/if}
{:else if isPreviewableImage(file.name)}
  <a href={files.previewUrl(file.path)} target="_blank" rel="noopener noreferrer" class="block">
    <img src={files.previewUrl(file.path)} alt={file.name} loading="lazy" class="max-h-60 max-w-full rounded" />
  </a>
{:else if isPlayableVideo(file.name)}
  <!-- svelte-ignore a11y_media_has_caption -->
  <video src={files.previewUrl(file.path)} controls preload="none" class="max-h-60 max-w-full rounded"></video>
{:else if isPlayableAudio(file.name)}
  <audio src={files.previewUrl(file.path)} controls preload="none" class="max-w-full"></audio>
{:else}
  <button type="button" class="btn btn-sm max-w-full justify-start" onclick={() => save(file.path!)}>
    <Icon name="download" size={14} />
    <span class="truncate">{file.name}</span>
    {#if file.size}<span class="opacity-60">{formatBytes(file.size)}</span>{/if}
  </button>
  <ErrorLine {error} size="xs" />
{/if}
