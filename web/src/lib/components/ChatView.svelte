<script lang="ts">
  // A conversation, the latest message at the bottom; older ones page in as it
  // scrolls up. The column is reversed, so a page added on top leaves the view
  // where it was.
  import { onMount } from 'svelte';
  import type { ChatEvent, ChatMessage, Participant, Picture } from '../api/backup-contents';
  import { errMsg } from '../api/client';
  import type { FileSource } from '../api/files';
  import { formatSeconds } from '../format';
  import Avatar from './Avatar.svelte';
  import CallIcon from './CallIcon.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import FileAttachment from './FileAttachment.svelte';
  import Modal from './Modal.svelte';

  let { title, subtitle, avatar, picture, load, files, members, onclose }: {
    title: string;
    subtitle: string;
    /** The chat's picture. */
    avatar?: string;
    /** A member's picture. */
    picture: (p?: Picture) => string | undefined;
    load: (offset: number, limit: number, signal: AbortSignal) => Promise<ChatMessage[]>;
    /** Where the attachments are, by path. */
    files: FileSource;
    /** A group's members, who name and picture its incoming messages; a one-to-one chat goes without. */
    members?: Participant[];
    onclose: () => void;
  } = $props();

  const PAGE = 100;
  let messages = $state.raw<ChatMessage[]>([]);
  let loading = $state(false);
  let done = $state(false);
  let error = $state<string | null>(null);
  let scroller = $state<HTMLElement | null>(null);
  let sentinel = $state<HTMLElement | null>(null);
  const ctrl = new AbortController();

  async function more(): Promise<void> {
    if (loading || done) return;
    loading = true;
    try {
      const page = await load(messages.length, PAGE, ctrl.signal);
      messages = [...messages, ...page];
      done = page.length < PAGE;
    } catch (err) {
      if (!ctrl.signal.aborted) error = errMsg(err, 'unknown_error');
    } finally {
      loading = false;
    }
  }

  // As on the phone: received bubbles are gray, sent ones in their service's
  // color, and a label marks where Messages changes service.
  const SENT: Record<string, string> = {
    iMessage: 'chat-bubble-imessage',
    SMS: 'chat-bubble-sms',
    RCS: 'chat-bubble-sms',
    WhatsApp: 'chat-bubble-whatsapp',
  };
  const LABELS: Record<string, string> = { iMessage: 'iMessage', SMS: 'Text Message · SMS', RCS: 'Text Message · RCS' };
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

  const textOf = (m: ChatMessage) => `${m.kind === 'poll' ? '📊 ' : m.kind === 'contact' ? '👤 ' : ''}${m.text ?? ''}`;

  const mapUrl = (l: NonNullable<ChatMessage['location']>) =>
    `https://www.openstreetmap.org/?mlat=${l.latitude}&mlon=${l.longitude}#map=16/${l.latitude}/${l.longitude}`;

  // Only what renders counts for the day dividers, service labels and runs;
  // paging counts every message.
  const shows = (m: ChatMessage) =>
    m.event ? !!eventText(m.event) : !!(m.text || m.attachments?.length || m.call || m.location || placeholder(m));
  const shown = $derived(messages.filter(shows));

  // As in WhatsApp, a member's messages in a row share a colored name, atop the
  // first, and a picture, beside the last; the column runs newest first.
  const member = (address?: string) => members?.find((p) => p.address === address);
  const sameAuthor = (a?: ChatMessage, b?: ChatMessage) =>
    !!a && !!b && !a.fromMe && !b.fromMe && !a.event && !b.event && a.sender === b.sender;
  const NAME_COLORS = ['text-primary', 'text-info', 'text-success', 'text-accent'];
  const nameColor = (address = '') =>
    NAME_COLORS[[...address].reduce((sum, c) => sum + c.charCodeAt(0), 0) % NAME_COLORS.length];

  const dayOf = (m?: ChatMessage) => (m ? new Date(m.time).toDateString() : '');
  const dayLabel = (iso: string) =>
    new Date(iso).toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'long', year: 'numeric' });
  const timeLabel = (iso: string) => new Date(iso).toLocaleTimeString(undefined, { timeStyle: 'short' });

  onMount(() => {
    void more();
    return () => ctrl.abort();
  });

  // Older messages page in when the top nears; a fresh observer per page
  // pulls again while the top stays in view.
  $effect(() => {
    if (!sentinel || !scroller) return;
    const io = new IntersectionObserver(
      ([entry]) => {
        if (entry.isIntersecting) void more();
      },
      { root: scroller, rootMargin: '400px' },
    );
    io.observe(sentinel);
    return () => io.disconnect();
  });
</script>

<Modal {title} {subtitle} closable size="wide" class="gap-3" {onclose}>
  {#snippet leading()}
    <Avatar src={avatar} name={title} class="w-10" />
  {/snippet}
  <div class="flex min-h-0 flex-1 flex-col-reverse overflow-auto rounded-box bg-base-200 p-3" bind:this={scroller}>
    {#each shown as m, i (m.id)}
      {#if m.event}
        <p class="py-1 text-center text-xs text-base-content/60">{eventText(m.event)}</p>
      {:else}
        {@const author = member(m.sender)}
        <div class={`chat ${m.fromMe ? 'chat-end' : 'chat-start'}`}>
          {#if members && !m.fromMe}
            <div class="chat-image">
              {#if sameAuthor(shown[i - 1], m)}
                <div class="w-8"></div>
              {:else}
                <Avatar src={picture(author)} name={author?.name ?? ''} class="w-8" />
              {/if}
            </div>
          {/if}
          <div class={`chat-bubble flex min-w-0 flex-col gap-1 ${bubble(m)}`}>
            {#if members && !m.fromMe && !sameAuthor(m, shown[i + 1])}
              <span class={`text-xs font-semibold ${nameColor(m.sender)}`}>{author?.name || m.sender || 'Unknown'}</span>
            {/if}
            {#if m.call}
              <span class="flex items-center gap-2"><CallIcon call={m.call} size={15} />{callLabel(m.call)}</span>
            {:else if m.location}
              <a href={mapUrl(m.location)} target="_blank" rel="noopener noreferrer" class="link">
                📍 {m.location.name || `${m.location.latitude.toFixed(5)}, ${m.location.longitude.toFixed(5)}`}
              </a>
            {:else if placeholder(m)}
              <p class="italic opacity-70">{placeholder(m)}</p>
            {/if}
            {#each m.attachments ?? [] as a, j (j)}
              <FileAttachment file={a} {files} />
            {/each}
            {#if m.text}
              <p class="whitespace-pre-wrap wrap-anywhere">{textOf(m)}</p>
            {/if}
          </div>
          <div class="chat-footer opacity-50"><time datetime={m.time}>{timeLabel(m.time)}</time></div>
        </div>
      {/if}
      {#if m.service && LABELS[m.service] && m.service !== shown[i + 1]?.service}
        <p class="pt-1 text-center text-[11px] font-medium text-base-content/50">{LABELS[m.service]}</p>
      {/if}
      {#if dayOf(m) !== dayOf(shown[i + 1])}
        <div class="divider my-2 text-xs text-base-content/50">{dayLabel(m.time)}</div>
      {/if}
    {/each}
    {#if loading}
      <p class="flex justify-center p-3"><span class="loading loading-spinner loading-sm"></span></p>
    {:else if error}
      <!-- At the top, where the page that failed belongs. -->
      <div class="flex flex-col items-center gap-2 p-3">
        <ErrorLine {error} variant="alert" />
        <button
          type="button"
          class="btn btn-ghost btn-xs"
          onclick={() => {
            error = null;
            void more();
          }}>Retry</button
        >
      </div>
    {:else if done && shown.length === 0}
      <p class="p-4 text-sm text-base-content/50">No messages</p>
    {:else if !done}
      <div bind:this={sentinel} class="h-px shrink-0"></div>
    {/if}
  </div>
</Modal>
