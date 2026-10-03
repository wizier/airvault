<script lang="ts">
  import { backupFiles, listBackupChats, listBackupMessages, type BackupChat, type ChatApp } from '../api/backup-contents';
  import { formatDateTime } from '../format';
  import Avatar from './Avatar.svelte';
  import ChatView from './ChatView.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, app, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    app: ChatApp;
    onclose: () => void;
  } = $props();

  const files = $derived(backupFiles(snapshotId, app));

  let open = $state<BackupChat | null>(null);

  function chatSubtitle(chat: BackupChat): string {
    const people = chat.participants ?? [];
    return people.length > 1 ? `${people.length} people` : (people[0]?.address ?? '');
  }

</script>

<SearchList
  {title}
  {subtitle}
  noun="chats"
  placeholder="Search names and numbers"
  load={(signal) => listBackupChats(snapshotId, app, signal)}
  text={(c) => [c.title, ...(c.participants ?? []).flatMap((p) => [p.name, p.address])]}
  {onclose}
>
  {#snippet row(c)}
    <button type="button" class="list-col-grow flex min-w-0 items-center gap-3 text-left" onclick={() => (open = c)}>
      <Avatar src={c.avatar && files.previewUrl(c.avatar)} name={c.title} />
      <span class="min-w-0 flex-1">
        <span class="block truncate font-medium">{c.title}</span>
        <span class="block truncate text-xs text-base-content/50">
          {c.snippet || (c.messages ? `${c.messages.toLocaleString()} messages` : '')}
        </span>
      </span>
      <span class="shrink-0 text-xs text-base-content/50">{formatDateTime(c.last)}</span>
    </button>
  {/snippet}
</SearchList>

{#if open}
  {@const chat = open}
  <ChatView
    title={chat.title}
    subtitle={chatSubtitle(chat)}
    load={(offset, limit, signal) => listBackupMessages(snapshotId, app, chat, offset, limit, signal)}
    {files}
    members={(chat.participants?.length ?? 0) > 1 ? chat.participants : undefined}
    onclose={() => (open = null)}
  />
{/if}
