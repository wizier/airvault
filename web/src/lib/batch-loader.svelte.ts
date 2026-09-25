// Coalesces per-item requests (gallery thumbnails, app icons) into batched
// POSTs: keys queued within a short debounce go out in fixed-size batches, all
// under one AbortSignal that reset() cancels. nearViewport picks the keys.

export interface BatchLoader {
  /** Queue a key unless it is already queued or in flight. The caller skips loaded keys. */
  queue(key: string): void;
  /** Abort the batches in flight and forget queued keys. */
  reset(): void;
}

interface BatchLoaderOptions<T> {
  batchSize: number;
  debounceMs: number;
  fetchBatch: (keys: string[], signal: AbortSignal) => Promise<Record<string, T>>;
  /** Called once per successful batch; keys missing from results have no value. */
  onBatch: (keys: string[], results: Record<string, T>) => void;
}

export function createBatchLoader<T>(options: BatchLoaderOptions<T>): BatchLoader {
  let ctrl = new AbortController();
  let queued: string[] = [];
  const pending = new Set<string>();
  let flushTimer = 0;

  function flush(): void {
    flushTimer = 0;
    const signal = ctrl.signal;
    const keys = queued;
    queued = [];
    for (let i = 0; i < keys.length; i += options.batchSize) {
      const batch = keys.slice(i, i + options.batchSize);
      for (const key of batch) pending.add(key);
      void options
        .fetchBatch(batch, signal)
        .then((results) => {
          if (!signal.aborted) options.onBatch(batch, results);
        })
        .catch(() => {
          /* transient batch failure: those keys retry when queued again */
        })
        .finally(() => {
          for (const key of batch) pending.delete(key);
        });
    }
  }

  return {
    queue(key) {
      if (pending.has(key) || queued.includes(key)) return;
      queued.push(key);
      // The debounce lets a burst of queued keys land in full batches.
      if (!flushTimer) flushTimer = window.setTimeout(flush, options.debounceMs);
    },
    reset() {
      ctrl.abort();
      ctrl = new AbortController();
      clearTimeout(flushTimer);
      flushTimer = 0;
      queued = [];
      pending.clear();
    },
  };
}

/** Tracks which items sit within rootMargin of a scroll container's visible box.
 *  Attach `root` to the container and `item(key)` to each item; onChange reports
 *  a key entering (true) or leaving the margin, and a detached item as leaving. */
export function nearViewport(rootMargin: string, onChange: (key: string, near: boolean) => void) {
  // Items attached before the root re-run once the observer exists.
  let observer = $state<IntersectionObserver | null>(null);
  const keys = new WeakMap<Element, string>();
  return {
    root(container: Element) {
      const io = new IntersectionObserver(
        (entries) => {
          for (const entry of entries) {
            const key = keys.get(entry.target);
            if (key !== undefined) onChange(key, entry.isIntersecting);
          }
        },
        { root: container, rootMargin },
      );
      observer = io;
      return () => {
        io.disconnect();
        observer = null;
      };
    },
    item(key: string) {
      return (el: Element) => {
        const io = observer;
        if (!io) return;
        keys.set(el, key);
        io.observe(el);
        return () => {
          io.unobserve(el);
          onChange(key, false);
        };
      };
    },
  };
}
