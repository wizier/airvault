<script lang="ts">
  import { backupFiles, listBackupNotes, type BackupAttachment, type BackupNote } from '../api/backup-contents';
  import { formatDateTime } from '../format';
  import FileAttachment from './FileAttachment.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    onclose: () => void;
  } = $props();

  let open = $state<BackupNote | null>(null);
  const files = $derived(backupFiles(snapshotId, 'notes'));
  // Where an attachment sits in a note's text.
  const PLACEHOLDER = '\uFFFC';
  const when = (n: BackupNote) => (n.modified ? formatDateTime(n.modified) : '');

  // The text starts with the title, as in Notes; the line after it previews.
  const preview = (n: BackupNote) =>
    (n.text ?? '')
      .replaceAll(PLACEHOLDER, '')
      .split('\n')
      .slice(1)
      .find((line) => line.trim()) ?? '';
</script>

<SearchList
  {title}
  {subtitle}
  noun="notes"
  placeholder="Search notes"
  load={(signal) => listBackupNotes(snapshotId, signal)}
  text={(n) => [n.title, n.folder, n.text]}
  {onclose}
>
  {#snippet row(n)}
    <button type="button" class="list-col-grow min-w-0 text-left" onclick={() => (open = n)}>
      <p class="flex items-center gap-1.5 truncate font-medium">
        {#if n.locked}<Icon name="lock" size={13} class="shrink-0 text-base-content/50" />{/if}
        <span class="truncate">{n.title || 'New Note'}</span>
      </p>
      <p class="truncate text-xs text-base-content/50">
        {[when(n), n.folder, preview(n)].filter(Boolean).join(' · ')}
      </p>
    </button>
  {/snippet}
</SearchList>

<!-- A scanned document shows its pages in its place. -->
{#snippet attachment(a: BackupAttachment)}
  {#each a.pages?.length ? a.pages : [a] as file, i (i)}
    <div class="my-2 whitespace-normal"><FileAttachment {file} {files} /></div>
  {/each}
{/snippet}

{#if open}
  <Modal
    title={open.title || 'New Note'}
    subtitle={[open.folder, when(open)].filter(Boolean).join(' · ')}
    closable
    class="flex max-h-[85vh] max-w-xl flex-col gap-3"
    onclose={() => (open = null)}
  >
    <div class="min-h-0 overflow-auto rounded-box bg-base-200 p-4 text-sm">
      {#if open.locked}
        <p class="flex items-center gap-2 text-base-content/60">
          <Icon name="lock" size={14} /> This note is locked with its own password; the backup keeps it encrypted.
        </p>
      {:else}
        <div class="whitespace-pre-wrap wrap-anywhere">
          {#each (open.text ?? '').split(PLACEHOLDER) as piece, i (i)}{piece}{#if open.attachments?.[i]}{@render attachment(
                open.attachments[i],
              )}{/if}{/each}
        </div>
      {/if}
    </div>
  </Modal>
{/if}
