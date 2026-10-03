<script lang="ts">
  import { attachmentFiles, listBackupChats, listChatMessages, type BackupChat } from '../api/backups';
  import { formatDateTime } from '../format';
  import ChatView from './ChatView.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    onclose: () => void;
  } = $props();

  let open = $state<BackupChat | null>(null);

  function chatSubtitle(chat: BackupChat): string {
    const people = chat.participants ?? [];
    return people.length > 1 ? `${people.length} people` : (people[0]?.address ?? '');
  }

  // A group names who wrote each message; a one-to-one chat needs no names.
  function senderName(chat: BackupChat): ((address?: string) => string) | undefined {
    const people = chat.participants ?? [];
    if (people.length < 2) return undefined;
    return (address) => people.find((p) => p.address === address)?.name || address || 'Unknown';
  }
</script>

<SearchList
  {title}
  {subtitle}
  noun="chats"
  placeholder="Search names and numbers"
  load={(signal) => listBackupChats(snapshotId, signal)}
  text={(c) => [c.title, ...(c.participants ?? []).flatMap((p) => [p.name, p.address])]}
  {onclose}
>
  {#snippet row(c)}
    <button type="button" class="list-col-grow flex min-w-0 items-baseline gap-3 text-left" onclick={() => (open = c)}>
      <span class="min-w-0 flex-1">
        <span class="block truncate font-medium">{c.title}</span>
        <span class="block truncate text-xs text-base-content/50">
          {c.snippet || `${c.messages.toLocaleString()} messages`}
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
    load={(offset, limit, signal) => listChatMessages(snapshotId, chat, offset, limit, signal)}
    files={attachmentFiles(snapshotId)}
    senderName={senderName(chat)}
    onclose={() => (open = null)}
  />
{/if}
