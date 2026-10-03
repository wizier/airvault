<script lang="ts">
  // A conversation, the latest message at the bottom: older ones page in above,
  // later ones below a match further back. The column is reversed, so a page
  // added on top leaves the view where it was.
  import { onMount, tick } from 'svelte';
  import type { ChatMatch, ChatMessage, Participant, Picture } from '../api/backup-contents';
  import { errMsg } from '../api/client';
  import type { FileSource } from '../api/files';
  import { pullNear } from '../pull-near';
  import { debouncedSearch } from '../search.svelte';
  import Avatar from './Avatar.svelte';
  import ChatItem, { shows } from './ChatItem.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';

  let { title, subtitle, avatar, picture, load, search, found, files, members, onclose }: {
    title: string;
    subtitle: string;
    /** The chat's picture. */
    avatar?: string;
    /** A member's picture. */
    picture: (p?: Picture) => string | undefined;
    load: (offset: number, limit: number, signal: AbortSignal) => Promise<ChatMessage[]>;
    search: (query: string, signal: AbortSignal) => Promise<ChatMatch[]>;
    /** What a search of all the chats found here: it opens searched for, at that message. */
    found?: { term: string; id: number };
    /** Where the attachments are, by path. */
    files: FileSource;
    /** A group's members, who name and picture its incoming messages; a one-to-one chat goes without. */
    members?: Participant[];
    onclose: () => void;
  } = $props();

  const PAGE = 100;
  let messages = $state.raw<ChatMessage[]>([]);
  let first = $state(0); // where messages[0] stands, the latest being 0
  let loading = $state(false);
  let loadingLater = $state(false);
  let done = $state(false);
  let error = $state<string | null>(null);
  let scroller: HTMLElement | undefined;
  let pages = new AbortController(); // the loads of the messages in view

  async function older(): Promise<void> {
    if (loading || done) return;
    loading = true;
    const { signal } = pages;
    try {
      const page = await load(first + messages.length, PAGE, signal);
      if (signal.aborted) return;
      messages = [...messages, ...page];
      done = page.length < PAGE;
    } catch (err) {
      if (!signal.aborted) error = errMsg(err, 'unknown_error');
    } finally {
      if (!signal.aborted) loading = false;
    }
  }

  // Later messages go under the view, which the column scrolls from: it is
  // moved up by what they add, so what was in view stays.
  async function later(): Promise<void> {
    if (loadingLater || first === 0 || !scroller) return;
    loadingLater = true;
    const { signal } = pages;
    try {
      const count = Math.min(PAGE, first);
      const page = await load(first - count, count, signal);
      if (signal.aborted) return;
      const height = scroller.scrollHeight;
      messages = [...page, ...messages];
      first -= count;
      await tick();
      scroller.scrollTop -= scroller.scrollHeight - height;
    } catch (err) {
      if (!signal.aborted) error = errMsg(err, 'unknown_error');
    } finally {
      if (!signal.aborted) loadingLater = false;
    }
  }

  // Loads the messages afresh around where offset stands, dropping any load
  // still running.
  async function show(offset: number): Promise<void> {
    pages.abort();
    pages = new AbortController();
    messages = [];
    first = Math.max(0, offset - PAGE / 2);
    done = false;
    loading = false;
    loadingLater = false;
    error = null;
    await older();
  }

  // Brings a match into view, loading around it when it is not loaded.
  async function go(match: ChatMatch): Promise<void> {
    if (match.offset < first || match.offset >= first + messages.length) await show(match.offset);
    await tick();
    scroller?.querySelector(`[data-id="${match.id}"]`)?.scrollIntoView({ block: 'center' });
  }

  // The search in the chat, its matches the latest first. One of all the
  // chats lands it at what it found, the first time.
  // svelte-ignore state_referenced_locally
  let landing = found;
  let searching = $state(!!landing);
  let term = $state(landing?.term ?? '');
  let current = $state(0); // the match in view
  const searched = debouncedSearch(
    () => (searching ? term : ''),
    async (query, signal) => {
      const matches = await search(query, signal);
      if (!signal.aborted) {
        current = Math.max(0, matches.findIndex((m) => m.id === landing?.id));
        landing = undefined;
        if (matches.length) void go(matches[current]);
      }
      return matches;
    },
    (query) => (query === landing?.term ? 0 : 300),
  );
  const matches = $derived(searched.result?.value);
  const marked = $derived(matches?.length ? searched.result?.query : '');

  function step(by: number): void {
    if (!matches?.length) return;
    current = Math.min(Math.max(current + by, 0), matches.length - 1);
    void go(matches[current]);
  }

  // Only what shows counts for the day dividers, service labels and runs;
  // paging counts every message.
  const shown = $derived(messages.filter(shows));

  // As in WhatsApp, a member's messages in a row share a name, atop the first,
  // and a picture, beside the last; the column runs newest first.
  const member = (address?: string) => members?.find((p) => p.address === address);
  const sameAuthor = (a?: ChatMessage, b?: ChatMessage) =>
    !!a && !!b && !a.fromMe && !b.fromMe && !a.event && !b.event && a.sender === b.sender;

  // A label marks where Messages changes service.
  const LABELS: Record<string, string> = { iMessage: 'iMessage', SMS: 'Text Message · SMS', RCS: 'Text Message · RCS' };
  const dayOf = (m?: ChatMessage) => (m ? new Date(m.time).toDateString() : '');
  const dayLabel = (iso: string) =>
    new Date(iso).toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'long', year: 'numeric' });

  onMount(() => {
    void older();
    return () => pages.abort();
  });
</script>

<Modal {title} {subtitle} closable size="wide" class="gap-3" {onclose}>
  {#snippet leading()}
    <Avatar src={avatar} name={title} class="w-10" />
  {/snippet}
  {#snippet headerActions()}
    <button
      type="button"
      class={`btn btn-square btn-ghost btn-sm ${searching ? 'btn-active' : ''}`}
      aria-label="Search in this chat"
      aria-pressed={searching}
      onclick={() => (searching = !searching)}
    >
      <Icon name="search" size={16} />
    </button>
  {/snippet}
  {#if searching}
    <div class="flex shrink-0 items-center gap-2">
      <!-- Focused once the dialog opens, which takes the focus itself. -->
      <input
        type="search"
        class="input input-sm min-w-0 flex-1"
        placeholder="Search in this chat"
        bind:value={term}
        {@attach (input) => queueMicrotask(() => input.focus())}
        onkeydown={(e) => {
          if (e.key === 'Enter' && !e.isComposing) step(e.shiftKey ? -1 : 1);
          if (e.key === 'Escape') {
            e.preventDefault();
            searching = false;
          }
        }}
      />
      <span class="shrink-0 text-xs text-base-content/60 tabular-nums">
        {#if matches}
          {matches.length ? `${current + 1} of ${matches.length}` : 'No matches'}
        {:else if searched.active && !searched.result}
          <span class="loading loading-spinner loading-xs"></span>
        {/if}
      </span>
      <div class="join">
        <button
          type="button"
          class="btn btn-square btn-sm join-item"
          aria-label="Earlier match"
          disabled={!matches || current >= matches.length - 1}
          onclick={() => step(1)}><Icon name="chevronUp" size={16} /></button
        >
        <button
          type="button"
          class="btn btn-square btn-sm join-item"
          aria-label="Later match"
          disabled={!matches || current <= 0}
          onclick={() => step(-1)}><Icon name="chevronDown" size={16} /></button
        >
      </div>
    </div>
    <ErrorLine error={searched.result?.error ?? null} size="xs" />
  {/if}
  <!-- The browser's own scroll anchoring would move the view twice as later messages come in. -->
  <div
    class="flex min-h-0 flex-1 flex-col-reverse overflow-auto rounded-box bg-base-200 p-3 [overflow-anchor:none]"
    bind:this={scroller}
  >
    <!-- No spinner here: what it added under the view would move it. -->
    {#if first > 0 && messages.length && !loadingLater && !error}
      <div {@attach pullNear(later)} class="h-px shrink-0"></div>
    {/if}
    {#each shown as m, i (m.id)}
      <ChatItem
        message={m}
        byline={members && !m.fromMe && !m.event
          ? { author: member(m.sender), named: !sameAuthor(m, shown[i + 1]), pictured: !sameAuthor(shown[i - 1], m) }
          : undefined}
        {picture}
        {files}
        term={marked}
      />
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
      <!-- At the top, where the page that failed belongs; the pages pull again once it clears. -->
      <div class="flex flex-col items-center gap-2 p-3">
        <ErrorLine {error} variant="alert" />
        <button type="button" class="btn btn-ghost btn-xs" onclick={() => (error = null)}>Retry</button>
      </div>
    {:else if done && shown.length === 0}
      <p class="p-4 text-sm text-base-content/50">No messages</p>
    {:else if !done}
      <div {@attach pullNear(older)} class="h-px shrink-0"></div>
    {/if}
  </div>
</Modal>
