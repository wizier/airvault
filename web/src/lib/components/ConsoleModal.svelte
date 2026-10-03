<script lang="ts">
  // Capture fills a capped ring; the level and text filters decide what the view
  // shows and what eviction spares. As in Console.app and Logcat, Pause stops the
  // stream, and scrolling up stops following the tail until Latest.
  import { consoleUrl, type ConsoleLevel, type ConsoleLine } from '../api/console';
  import { errorText } from '../error-text';
  import Alert from './Alert.svelte';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';

  const MAX_LINES = 2000;
  /** The os_trace stream is a firehose (hundreds of records/sec); batching caps
   *  it at ≤5 renders/sec. */
  const FLUSH_MS = 200;

  let { udid, name, onclose }: { udid: string; name: string; onclose: () => void } = $props();

  /** A stable identity, so the keyed {#each} only touches appended/dropped rows. */
  type Row = ConsoleLine & { seq: number };

  let lines = $state.raw<Row[]>([]);
  /** Closes the stream, so the phone stops sending; Resume opens a new session. */
  let paused = $state(false);
  let filter = $state('');
  let minLevel = $state<'all' | 'notice' | 'errors'>('all');
  let streamError = $state<string | null>(null);
  let connected = $state(false);
  let logEl = $state<HTMLElement | null>(null);
  /** Bumped by Reconnect: reopens the stream, deliberately keeping the
   *  buffered rows — what happened right before a drop is worth reading. */
  let streamNonce = $state(0);
  let following = $state(true);

  /** Monotonic row id — component-scoped so rows retained across a reconnect or
   *  pause never collide with the resumed stream's new ones. */
  let seq = 0;

  // A string: the parent's device object is replaced on every list refresh, and
  // an unchanged URL must not reopen the stream (a new os_trace session).
  const url = $derived(consoleUrl(udid));

  // One EventSource per open modal (auto-reconnects; backend failures arrive as
  // the 'error' event). Incoming records collect in a PLAIN buffer and hit the
  // reactive state on a timer — one render per flush, never one per record.
  $effect(() => {
    void streamNonce; // dependency: Reconnect re-runs this effect
    if (paused) return;
    streamError = null;

    let pending: Row[] = [];
    let flushTimer: ReturnType<typeof setTimeout> | null = null;

    function flush() {
      flushTimer = null;
      lines = trimToBudget([...lines, ...pending]);
      pending = [];
    }

    const es = new EventSource(url);
    es.onopen = () => {
      connected = true;
      streamError = null;
    };
    es.onmessage = (e) => {
      try {
        const row = JSON.parse(e.data) as Row;
        row.seq = ++seq;
        pending.push(row);
      } catch {
        return;
      }
      if (flushTimer === null) flushTimer = setTimeout(flush, FLUSH_MS);
    };
    es.addEventListener('error', (e) => {
      const msg = (e as MessageEvent).data;
      if (typeof msg === 'string' && msg) {
        // In-band backend failure (device offline, session died). FATAL: stop
        // the EventSource — otherwise it auto-reconnects every few seconds,
        // re-attempting a device session forever. Reconnect is manual.
        let code = 'console_stream_failed';
        try {
          const payload = JSON.parse(msg) as { code?: unknown };
          if (typeof payload.code === 'string') code = payload.code;
        } catch {
          /* keep the generic code */
        }
        streamError = errorText(code, 'console_stream_failed');
        es.close();
      }
      connected = false;
    });
    // Closing drops the batch not yet shown: a pause leaves the view as it was.
    return () => {
      es.close();
      if (flushTimer !== null) clearTimeout(flushTimer);
      connected = false;
    };
  });

  const HIDDEN_BY_LEVEL: Record<'all' | 'notice' | 'errors', ConsoleLevel[]> = {
    all: [],
    notice: ['debug', 'info'],
    errors: ['debug', 'info', 'notice'],
  };

  function matchesFilter(l: ConsoleLine, needle: string): boolean {
    return (
      l.message.toLowerCase().includes(needle) ||
      l.image.toLowerCase().includes(needle) ||
      (l.subsystem ?? '').toLowerCase().includes(needle)
    );
  }

  const filtered = $derived(filter !== '' || minLevel !== 'all');
  const shown = $derived.by(() => {
    const hidden = HIDDEN_BY_LEVEL[minLevel];
    const needle = filter.toLowerCase();
    return (l: ConsoleLine) => !hidden.includes(l.level) && (!needle || matchesFilter(l, needle));
  });

  /** Ring budget: the oldest rows the filters hide go first, so a filtered view
   *  keeps scrolling; shown rows yield only once they alone exceed MAX_LINES. */
  function trimToBudget(rows: Row[]): Row[] {
    let overflow = rows.length - MAX_LINES;
    if (overflow <= 0) return rows;
    const kept: Row[] = [];
    for (const r of rows) {
      if (overflow > 0 && !shown(r)) overflow--;
      else kept.push(r);
    }
    return kept.slice(-MAX_LINES);
  }

  const visible = $derived(filtered ? lines.filter(shown) : lines);

  const LEVEL_CLASS: Record<ConsoleLevel, string> = {
    fault: 'text-error font-semibold',
    error: 'text-error',
    notice: '',
    info: 'text-base-content/70',
    debug: 'text-base-content/40',
  };

  $effect(() => {
    void visible;
    if (following && logEl) logEl.scrollTop = logEl.scrollHeight;
  });

  function onLogScroll() {
    if (!logEl) return;
    following = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 24;
  }

  function togglePause() {
    paused = !paused;
    if (!paused) following = true;
  }

  function clear() {
    lines = [];
    following = true;
  }
</script>

<!-- Fixed height: a console is a terminal window — its size must not
     breathe with how many rows currently match. -->
<Modal subtitle={`Live system log of ${name}`} closable class="flex h-[85vh] w-11/12 max-w-5xl flex-col gap-3" {onclose}>
  {#snippet heading()}
    <Icon name="terminal" size={18} />
    Console
    {#if connected || paused}
      <span
        class={`status ${paused ? 'status-warning' : 'status-success'}`}
        title={paused ? 'Paused' : 'Streaming'}
      ></span>
    {/if}
  {/snippet}

  <div class="flex flex-wrap items-center gap-2">
    <label class="input input-sm w-full sm:w-auto sm:min-w-40 sm:flex-1">
      <input type="text" placeholder="Filter message / process / subsystem…" bind:value={filter} />
      {#if filter}
        <button type="button" class="btn btn-circle btn-ghost btn-xs" aria-label="Clear filter" onclick={() => (filter = '')}>
          <Icon name="x" size={12} />
        </button>
      {/if}
    </label>
    <select class="select select-sm w-32 sm:w-36" bind:value={minLevel} aria-label="Level filter">
      <option value="all">All levels</option>
      <option value="notice">Notice &amp; up</option>
      <option value="errors">Errors only</option>
    </select>
    <button
      type="button"
      class={`btn btn-sm ${paused ? 'btn-warning' : 'btn-ghost'}`}
      onclick={togglePause}
      title={paused ? 'Resume the log stream from the phone' : 'Stop the log stream from the phone'}
    >
      <Icon name={paused ? 'play' : 'pause'} size={13} />
      <span class="hidden sm:inline">{paused ? 'Resume' : 'Pause'}</span>
    </button>
    <button type="button" class="btn btn-ghost btn-sm" title="Clear the buffered output" onclick={clear}>
      <Icon name="trash" size={13} /> <span class="hidden sm:inline">Clear</span>
    </button>
  </div>

  {#if streamError}
    <Alert tone="error" class="text-sm">
      {streamError}
      {#snippet actions()}
        <button type="button" class="btn btn-ghost btn-xs" onclick={() => (streamNonce += 1)}>
          <Icon name="refresh" size={12} />
          Reconnect
        </button>
      {/snippet}
    </Alert>
  {/if}

  <div class="relative min-h-0 flex-1">
    <div
      bind:this={logEl}
      onscroll={onLogScroll}
      class="h-full overflow-auto rounded-box bg-base-200 p-3 font-mono text-[11px] leading-relaxed"
    >
      {#if visible.length === 0}
        <p class="flex items-center gap-2 font-sans text-sm text-base-content/50">
          {#if filtered}
            No records match the filters
          {:else if paused}
            Paused
          {:else}
            <span class="loading loading-dots loading-xs"></span>
            Waiting for log output…
          {/if}
        </p>
      {:else}
        {#each visible as l (l.seq)}
          <div class={`whitespace-pre-wrap break-all ${LEVEL_CLASS[l.level]}`}>
            <span class="text-base-content/40">{l.ts}</span>
            <span class="text-base-content/60">{l.image}[{l.pid}]</span>
            {l.message}
          </div>
        {/each}
      {/if}
    </div>
    {#if !following}
      <button
        type="button"
        class="btn btn-neutral btn-sm absolute bottom-3 right-5 shadow"
        onclick={() => (following = true)}
      >
        <Icon name="arrowDown" size={13} /> Latest
      </button>
    {/if}
  </div>

  <p class="text-xs text-base-content/40">
    {#if filtered}
      {visible.length.toLocaleString()} of {lines.length.toLocaleString()} buffered records match —
      matching records are kept longest, the rest are dropped first.
    {:else}
      Keeps the last {MAX_LINES.toLocaleString()} records; older output is dropped.
    {/if}
  </p>
</Modal>
