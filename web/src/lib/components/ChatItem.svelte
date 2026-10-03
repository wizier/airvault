<script module lang="ts">
  import type { ChatEvent, ChatMessage, Participant } from '../api/backup-contents';
  import { formatSeconds } from '../format';

  // As on the phone: received bubbles are gray, sent ones in their service's color.
  const SENT: Record<string, string> = {
    iMessage: 'chat-bubble-imessage',
    SMS: 'chat-bubble-sms',
    RCS: 'chat-bubble-sms',
    WhatsApp: 'chat-bubble-whatsapp',
  };
  const bubble = (m: ChatMessage) => (m.fromMe ? (SENT[m.service ?? ''] ?? 'chat-bubble-primary') : 'chat-bubble-received');

  // What a message says in its bubble when it has neither text nor a place.
  const PLACEHOLDERS: Record<string, string> = {
    waiting: 'Waiting for this message',
    viewOncePhoto: 'View once photo',
    viewOnceVideo: 'View once video',
    viewOnceVoice: 'View once voice message',
    poll: 'Poll',
    contact: 'Contact',
    location: 'Location',
  };
  function placeholder(m: ChatMessage): string {
    if (m.text || m.location) return '';
    if (m.kind === 'deleted') return m.fromMe ? 'You deleted this message' : 'This message was deleted';
    return PLACEHOLDERS[m.kind ?? ''] ?? '';
  }

  function callLabel(call: NonNullable<ChatMessage['call']>): string {
    const what = call.video ? 'Video call' : 'Voice call';
    if (!call.answered) return call.outgoing ? `${what} · No answer` : `Missed ${what.toLowerCase()}`;
    return call.duration ? `${what} · ${formatSeconds(call.duration)}` : what;
  }

  const who = (p?: Participant) => (p ? p.name || p.address : 'You');
  function eventText(e: ChatEvent): string {
    const actor = who(e.actor);
    const targets = (e.targets ?? []).map(who).join(', ');
    switch (e.code) {
      case 'renamed':
        return `${actor} changed the group name to “${e.text ?? ''}”`;
      case 'added':
        return `${actor} added ${targets}`;
      case 'removed':
        return `${actor} removed ${targets}`;
      case 'left':
        return `${actor} left`;
      case 'joined':
        return `${actor} joined`;
      case 'created':
        return `${actor} created the group`;
      case 'photo':
        return `${actor} changed the group photo`;
      case 'photoRemoved':
        return `${actor} deleted the group photo`;
      case 'description':
        return `${actor} changed the group description`;
      case 'timer':
        return `${actor} turned ${e.text === '0' ? 'off' : 'on'} disappearing messages`;
      case 'number':
        return `${actor} changed their phone number`;
      case 'encrypted':
        return 'Messages are end-to-end encrypted';
      case 'security':
        return `${actor}'s security code changed`;
    }
    return '';
  }

  /** Whether a message shows anything at all. */
  export const shows = (m: ChatMessage) =>
    m.event ? !!eventText(m.event) : !!(m.text || m.attachments?.length || m.call || m.location || placeholder(m));

  const textOf = (m: ChatMessage) => `${m.kind === 'poll' ? '📊 ' : m.kind === 'contact' ? '👤 ' : ''}${m.text ?? ''}`;

  const mapUrl = (l: NonNullable<ChatMessage['location']>) =>
    `https://www.openstreetmap.org/?mlat=${l.latitude}&mlon=${l.longitude}#map=16/${l.latitude}/${l.longitude}`;

  const NAME_COLORS = ['text-primary', 'text-info', 'text-success', 'text-accent'];
  const nameColor = (address = '') =>
    NAME_COLORS[[...address].reduce((sum, c) => sum + c.charCodeAt(0), 0) % NAME_COLORS.length];

  const timeLabel = (iso: string) => new Date(iso).toLocaleTimeString(undefined, { timeStyle: 'short' });
</script>

<script lang="ts">
  // One message of a chat: a line about the chat, or a bubble.
  import type { Picture } from '../api/backup-contents';
  import type { FileSource } from '../api/files';
  import Avatar from './Avatar.svelte';
  import CallIcon from './CallIcon.svelte';
  import FileAttachment from './FileAttachment.svelte';
  import Marked from './Marked.svelte';

  let { message: m, byline, picture, files, term = '' }: {
    message: ChatMessage;
    /** Who wrote one coming in to a group: named atop a run of theirs, pictured beside its last. */
    byline?: { author?: Participant; named: boolean; pictured: boolean };
    picture: (p?: Picture) => string | undefined;
    files: FileSource;
    /** What a search found, marked. */
    term?: string;
  } = $props();
</script>

{#if m.event}
  <p class="py-1 text-center text-xs text-base-content/60" data-id={m.id}>{eventText(m.event)}</p>
{:else}
  <div class={`chat ${m.fromMe ? 'chat-end' : 'chat-start'}`} data-id={m.id}>
    {#if byline}
      <div class="chat-image">
        {#if byline.pictured}
          <Avatar src={picture(byline.author)} name={byline.author?.name ?? ''} class="w-8" />
        {:else}
          <div class="w-8"></div>
        {/if}
      </div>
    {/if}
    <div class={`chat-bubble flex min-w-0 flex-col gap-1 ${bubble(m)}`}>
      {#if byline?.named}
        <span class={`text-xs font-semibold ${nameColor(m.sender)}`}>{byline.author?.name || m.sender || 'Unknown'}</span>
      {/if}
      {#if m.call}
        <span class="flex items-center gap-2"><CallIcon call={m.call} size={15} />{callLabel(m.call)}</span>
      {:else if m.location}
        <a href={mapUrl(m.location)} target="_blank" rel="noopener noreferrer" class="link">
          📍 <Marked text={m.location.name || `${m.location.latitude.toFixed(5)}, ${m.location.longitude.toFixed(5)}`} {term} />
        </a>
      {:else if placeholder(m)}
        <p class="italic opacity-70">{placeholder(m)}</p>
      {/if}
      {#each m.attachments ?? [] as file, i (i)}
        <FileAttachment {file} {files} {term} />
      {/each}
      {#if m.text}
        <p class="whitespace-pre-wrap wrap-anywhere"><Marked text={textOf(m)} {term} /></p>
      {/if}
    </div>
    <div class="chat-footer opacity-50"><time datetime={m.time}>{timeLabel(m.time)}</time></div>
  </div>
{/if}
