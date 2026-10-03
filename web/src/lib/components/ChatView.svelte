<script lang="ts">
  // A conversation, the latest message at the bottom; older ones page in as it
  // scrolls up. The column is reversed, so a page added on top leaves the view
  // where it was.
  import { onMount } from 'svelte';
  import type { ChatAttachment, ChatMessage } from '../api/backups';
  import { errMsg } from '../api/client';
  import { downloadFile, type FileSource } from '../api/files';
  import { formatBytes } from '../format';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import { isPlayableVideo, isPreviewableImage } from './PreviewImage.svelte';

  let { title, subtitle, load, files, senderName, onclose }: {
    title: string;
    subtitle: string;
    load: (offset: number, limit: number, signal: AbortSignal) => Promise<ChatMessage[]>;
    /** Where the attachments are, by path. */
    files: FileSource;
    /** Names who sent an incoming message; a one-to-one chat goes without. */
    senderName?: (address?: string) => string;
    onclose: () => void;
  } = $props();

  const PAGE = 100;
  let messages = $state<ChatMessage[]>([]);
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

  async function save(a: ChatAttachment): Promise<void> {
    error = null;
    try {
      await downloadFile(files, a.path!, a.name, ctrl.signal);
    } catch (err) {
      if (!ctrl.signal.aborted) error = errMsg(err, 'download_failed');
    }
  }

  // As on the phone: sent bubbles are blue for iMessage and green for SMS and
  // RCS, received ones gray, and a label marks where the service changes.
  const bubble = (m: ChatMessage) =>
    !m.fromMe ? 'chat-bubble-received' : m.service === 'iMessage' ? 'chat-bubble-imessage' : 'chat-bubble-sms';
  const serviceLabel = (service: string) => (service === 'iMessage' ? service : `Text Message · ${service}`);
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

<Modal {title} {subtitle} closable class="flex h-[90vh] max-h-[90vh] w-full max-w-2xl flex-col gap-3" {onclose}>
  <div class="flex min-h-0 flex-1 flex-col-reverse overflow-auto rounded-box bg-base-200 p-3" bind:this={scroller}>
    <ErrorLine {error} variant="alert" />
    {#each messages as m, i (m.id)}
      {#if m.text || m.attachments?.length}
        <div class={`chat ${m.fromMe ? 'chat-end' : 'chat-start'}`}>
          {#if senderName && !m.fromMe && messages[i + 1]?.sender !== m.sender}
            <div class="chat-header text-base-content/50">{senderName(m.sender)}</div>
          {/if}
          <div class={`chat-bubble flex min-w-0 flex-col gap-1 ${bubble(m)}`}>
            {#each m.attachments ?? [] as a, j (j)}
              {#if a.missing || !a.path}
                <span class="flex items-center gap-1 text-xs opacity-70" title="Kept only in iCloud">
                  <Icon name="cloud" size={12} />
                  {a.name}
                </span>
              {:else if isPreviewableImage(a.name)}
                <a href={files.previewUrl(a.path)} target="_blank" rel="noopener" class="block">
                  <img src={files.previewUrl(a.path)} alt={a.name} loading="lazy" class="max-h-60 max-w-full rounded" />
                </a>
              {:else if isPlayableVideo(a.name)}
                <!-- svelte-ignore a11y_media_has_caption -->
                <video src={files.previewUrl(a.path)} controls preload="none" class="max-h-60 max-w-full rounded"></video>
              {:else}
                <button type="button" class="btn btn-sm max-w-full justify-start" onclick={() => save(a)}>
                  <Icon name="download" size={14} />
                  <span class="truncate">{a.name}</span>
                  <span class="opacity-60">{formatBytes(a.size)}</span>
                </button>
              {/if}
            {/each}
            {#if m.text}
              <p class="whitespace-pre-wrap wrap-anywhere">{m.text}</p>
            {/if}
          </div>
          <div class="chat-footer opacity-50"><time datetime={m.time}>{timeLabel(m.time)}</time></div>
        </div>
      {/if}
      {#if m.service && m.service !== messages[i + 1]?.service}
        <p class="pt-1 text-center text-[11px] font-medium text-base-content/50">{serviceLabel(m.service)}</p>
      {/if}
      {#if dayOf(m) !== dayOf(messages[i + 1])}
        <div class="divider my-2 text-xs text-base-content/50">{dayLabel(m.time)}</div>
      {/if}
    {/each}
    {#if loading}
      <p class="flex justify-center p-3"><span class="loading loading-spinner loading-sm"></span></p>
    {:else if done && messages.length === 0}
      <p class="p-4 text-sm text-base-content/50">No messages</p>
    {:else if !done && !error}
      <div bind:this={sentinel} class="h-px shrink-0"></div>
    {/if}
  </div>
</Modal>
