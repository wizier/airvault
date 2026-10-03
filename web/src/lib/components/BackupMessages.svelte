<script lang="ts">
  import {
    backupFiles,
    listBackupChats,
    listBackupMessages,
    pictureUrl,
    searchBackupChat,
    searchBackupMessages,
    type BackupChat,
    type ChatApp,
    type FoundMessage,
    type Picture,
  } from '../api/backup-contents';
  import { formatDateTime } from '../format';
  import Avatar from './Avatar.svelte';
  import ChatView from './ChatView.svelte';
  import Marked from './Marked.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, app, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    app: ChatApp;
    onclose: () => void;
  } = $props();

  const files = $derived(backupFiles(snapshotId, app));

  let chats = $state.raw<BackupChat[]>([]);
  // The chat open, searched for what found a message in it.
  let open = $state.raw<{ chat: BackupChat; found?: { term: string; id: number } } | null>(null);

  // A search's finds, each with its chat; one in a chat the list lacks is left out.
  type Hit = { chat: BackupChat; message: FoundMessage };
  async function find(query: string, signal: AbortSignal): Promise<Hit[]> {
    const found = await searchBackupMessages(snapshotId, app, query, signal);
    return found.flatMap((message) => {
      const chat = chats.find((c) => c.ids.includes(message.chat));
      return chat ? [{ chat, message }] : [];
    });
  }

  const picture = (p?: Picture) => pictureUrl(snapshotId, app, p);

  function chatSubtitle(chat: BackupChat): string {
    const people = chat.participants ?? [];
    return people.length > 1 ? `${people.length} people` : (people[0]?.address ?? '');
  }
</script>

<SearchList
  {title}
  {subtitle}
  noun="chats"
  placeholder="Search chats and messages"
  load={async (signal) => (chats = await listBackupChats(snapshotId, app, signal))}
  text={(c) => [c.title, ...(c.participants ?? []).flatMap((p) => [p.name, p.address])]}
  search={{ title: 'Messages', find, row: hitRow }}
  {onclose}
>
  {#snippet row(c)}
    <button type="button" class="list-col-grow flex min-w-0 items-center gap-3 text-left" onclick={() => (open = { chat: c })}>
      <Avatar src={picture(c)} name={c.title} />
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

{#snippet hitRow({ chat, message }: Hit, term: string)}
  <button
    type="button"
    class="list-col-grow flex min-w-0 items-center gap-3 text-left"
    onclick={() => (open = { chat, found: { term, id: message.id } })}
  >
    <Avatar src={picture(chat)} name={chat.title} />
    <span class="min-w-0 flex-1">
      <span class="block truncate font-medium">{chat.title}</span>
      <span class="block truncate text-xs text-base-content/60">
        {message.fromMe ? 'You: ' : ''}<Marked text={message.text ?? ''} {term} clipped />
      </span>
    </span>
    <span class="shrink-0 text-xs text-base-content/50">{formatDateTime(message.time)}</span>
  </button>
{/snippet}

{#if open}
  {@const { chat, found } = open}
  <ChatView
    title={chat.title}
    subtitle={chatSubtitle(chat)}
    avatar={picture(chat)}
    {picture}
    load={(offset, limit, signal) => listBackupMessages(snapshotId, app, chat, offset, limit, signal)}
    search={(query, signal) => searchBackupChat(snapshotId, app, chat, query, signal)}
    {found}
    {files}
    members={(chat.participants?.length ?? 0) > 1 ? chat.participants : undefined}
    onclose={() => (open = null)}
  />
{/if}
