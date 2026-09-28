<script lang="ts">
  // The level filter gates capture into the capped ring; the text filter shapes
  // the view and shields matches from eviction.
  import { consoleUrl, type ConsoleLevel, type ConsoleLine } from '../api/console';
  import { errorText } from '../error-text';
  import Icon from './Icon.svelte';

  const MAX_LINES = 2000;
  /** The os_trace stream is a firehose (hundreds of records/sec); batching caps
   *  it at ≤5 renders/sec. */
  const FLUSH_MS = 200;

  let { udid, name, onclose }: { udid: string; name: string; onclose: () => void } = $props();

  /** A stable identity, so the keyed {#each} only touches appended/dropped rows. */
  type Row = ConsoleLine & { seq: number };

  let lines = $state<Row[]>([]);
  let filter = $state('');
  let minLevel = $state<'all' | 'notice' | 'errors'>('all');
  // Streaming pause: on = records flow and the view auto-follows the tail; off =
  // incoming records are dropped and the view freezes so it can be read. The
  // EventSource stays connected either way, so resuming is instant.
  let streaming = $state(true);
  let streamError = $state<string | null>(null);
  let connected = $state(false);
  let logEl = $state<HTMLElement | null>(null);
  /** Bumped by Reconnect: reopens the stream, deliberately keeping the
   *  buffered rows — what happened right before a drop is worth reading. */
  let streamNonce = $state(0);
  let atBottom = $state(true);

  /** Monotonic row id — component-scoped so rows retained across a reconnect
   *  never collide with the resumed stream's new ones. */
  let seq = 0;

  let dialog: HTMLDialogElement;
  // A string: the parent's device object is replaced on every list refresh, and
  // an unchanged URL must not reopen the stream (a new os_trace session).
  const url = $derived(consoleUrl(udid));

  // One EventSource per open modal (auto-reconnects; backend failures arrive as
  // the 'error' event). Incoming records collect in a PLAIN buffer and hit the
  // reactive state on a timer — one render per flush, never one per record.
  $effect(() => {
    void streamNonce; // dependency: Reconnect re-runs this effect
    streamError = null;

    let pending: Row[] = [];
    let flushTimer: ReturnType<typeof setTimeout> | null = null;

    function flush() {
      flushTimer = null;
      // Re-check the level gate: it may have narrowed between capture and
      // flush (a ≤FLUSH_MS window) — without this, a few now-hidden rows
      // would slip into the buffer and sit there permanently.
      const hidden = HIDDEN_BY_LEVEL[minLevel];
      if (hidden.length > 0) pending = pending.filter((r) => !hidden.includes(r.level));
      if (pending.length === 0) return;
      // Append then trim: the eviction order (non-matching rows first) is
      // trimToBudget's job, even for a flush larger than MAX_LINES.
      for (const r of pending) lines.push(r);
      pending = [];
      trimToBudget();
    }

    const es = new EventSource(url);
    es.onopen = () => {
      connected = true;
      streamError = null;
    };
    es.onmessage = (e) => {
      if (!streaming) return; // paused: drop live records, freeze the view
      try {
        const row = JSON.parse(e.data) as Row;
        // The level gates capture (reads inside an event handler are not
        // effect dependencies, so changing it never reconnects the stream).
        if (HIDDEN_BY_LEVEL[minLevel].includes(row.level)) return;
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

  /** Narrowing the level drops already-captured rows below it; widening just
   *  captures more from now on (what was never captured can't be shown). */
  function pruneToLevel() {
    const hidden = HIDDEN_BY_LEVEL[minLevel];
    if (hidden.length > 0) lines = lines.filter((l) => !hidden.includes(l.level));
  }

  function matchesFilter(l: ConsoleLine, needle: string): boolean {
    return (
      l.message.toLowerCase().includes(needle) ||
      l.image.toLowerCase().includes(needle) ||
      (l.subsystem ?? '').toLowerCase().includes(needle)
    );
  }

  /** Ring budget: drop the oldest non-matching rows first so a filtered view
   *  keeps scrolling; matches yield only once they alone exceed MAX_LINES. */
  function trimToBudget() {
    let overflow = lines.length - MAX_LINES;
    if (overflow <= 0) return;
    const needle = filter.toLowerCase();
    if (needle) {
      // One pass: skip the oldest `overflow` non-matching rows, keep the rest.
      const kept: Row[] = [];
      for (const r of lines) {
        if (overflow > 0 && !matchesFilter(r, needle)) overflow--;
        else kept.push(r);
      }
      lines = kept;
      overflow = lines.length - MAX_LINES;
      if (overflow <= 0) return;
    }
    lines.splice(0, overflow); // matches alone still exceed the cap: drop the oldest
  }

  // The buffer is level-clean by construction (gated at capture, pruned on
  // narrowing) — the view only applies the text query.
  const visible = $derived.by(() => {
    const needle = filter.toLowerCase();
    if (!needle) return lines;
    return lines.filter((l) => matchesFilter(l, needle));
  });

  const LEVEL_CLASS: Record<ConsoleLevel, string> = {
    fault: 'text-error font-semibold',
    error: 'text-error',
    notice: '',
    info: 'text-base-content/70',
    debug: 'text-base-content/40',
  };

  // Follow the tail as lines arrive — but only while the view is pinned to the
  // bottom; scrolling up to read holds position. Pausing freezes everything.
  $effect(() => {
    void visible.length;
    if (streaming && atBottom && logEl) logEl.scrollTop = logEl.scrollHeight;
  });

  function onLogScroll() {
    if (!logEl) return;
    atBottom = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 24;
  }

  /** Resuming re-pins to the tail — the frozen view is stale by definition. */
  function toggleStreaming() {
    streaming = !streaming;
    if (streaming) {
      atBottom = true;
      if (logEl) logEl.scrollTop = logEl.scrollHeight;
    }
  }
</script>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} {onclose}>
  <!-- Fixed height: a console is a terminal window — its size must not
       breathe with how many rows currently match. -->
  <div class="modal-box flex h-[85vh] w-11/12 max-w-5xl flex-col gap-3">
    <div class="flex items-start justify-between gap-3">
      <div>
        <h3 class="flex items-center gap-2 text-lg font-bold">
          <Icon name="terminal" size={18} />
          Console
          {#if connected}
            <span
              class={`status ${streaming ? 'status-success' : 'status-warning'}`}
              title={streaming ? 'Streaming' : 'Paused'}
            ></span>
          {/if}
        </h3>
        <p class="mt-0.5 text-sm text-base-content/60">
          Live system log of {name}
        </p>
      </div>
      <button type="button" class="btn btn-square btn-ghost btn-sm" aria-label="Close" onclick={() => dialog.close()}>
        <Icon name="x" size={16} />
      </button>
    </div>

    <div class="flex flex-wrap items-center gap-2">
      <label class="input input-sm w-full sm:w-auto sm:min-w-40 sm:flex-1">
        <input type="text" placeholder="Filter message / process / subsystem…" bind:value={filter} />
        {#if filter}
          <button type="button" class="btn btn-circle btn-ghost btn-xs" aria-label="Clear filter" onclick={() => (filter = '')}>
            <Icon name="x" size={12} />
          </button>
        {/if}
      </label>
      <select
        class="select select-sm w-32 sm:w-36"
        bind:value={minLevel}
        onchange={pruneToLevel}
        aria-label="Level filter"
        title="Applies to records as they arrive"
      >
        <option value="all">All levels</option>
        <option value="notice">Notice &amp; up</option>
        <option value="errors">Errors only</option>
      </select>
      <button
        type="button"
        class={`btn btn-sm ${streaming ? 'btn-ghost' : 'btn-warning'}`}
        onclick={toggleStreaming}
        title={streaming ? 'Pause the stream to read the current output' : 'Resume live streaming'}
      >
        <Icon name={streaming ? 'pause' : 'play'} size={13} />
        <span class="hidden sm:inline">{streaming ? 'Pause' : 'Resume'}</span>
      </button>
      <button type="button" class="btn btn-ghost btn-sm" title="Clear the buffered output" onclick={() => (lines = [])}>
        <Icon name="trash" size={13} /> <span class="hidden sm:inline">Clear</span>
      </button>
    </div>

    {#if streamError}
      <div role="alert" class="alert alert-error alert-soft">
        <Icon name="alert" size={16} />
        <span class="text-sm">{streamError}</span>
        <button type="button" class="btn btn-ghost btn-xs" onclick={() => (streamNonce += 1)}>
          <Icon name="refresh" size={12} />
          Reconnect
        </button>
      </div>
    {/if}

    <div
      bind:this={logEl}
      onscroll={onLogScroll}
      class="min-h-0 flex-1 overflow-auto rounded-box bg-base-200 p-3 font-mono text-[11px] leading-relaxed"
    >
      {#if visible.length === 0}
        <p class="flex items-center gap-2 font-sans text-sm text-base-content/50">
          <span class="loading loading-dots loading-xs"></span>
          {filter || minLevel !== 'all' ? 'No records match the filters.' : 'Waiting for log output…'}
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

    <p class="text-xs text-base-content/40">
      {#if filter}
        {visible.length.toLocaleString()} of {lines.length.toLocaleString()} buffered records match — matching
        records are kept longest, unmatched noise is dropped first.
      {:else}
        Keeps the last {MAX_LINES.toLocaleString()} records passing the level filter; older output is dropped.
      {/if}
    </p>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>
