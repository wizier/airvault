import { errMsg } from './api/client';

/** What a search found for its query, or why it failed. */
export interface Searched<T> {
  query: string;
  value?: T;
  error?: string;
}

/** Runs find as query settles: two characters or more, a pause in typing, the
 *  latest query alone. `result` is for the query as it is now, null meanwhile. */
export function debouncedSearch<T>(
  query: () => string,
  find: (query: string, signal: AbortSignal) => Promise<T>,
  delay: (query: string) => number = () => 300,
): { readonly active: boolean; readonly result: Searched<T> | null } {
  let last = $state.raw<Searched<T> | null>(null);
  const sought = $derived(query().trim());
  const active = $derived([...sought].length >= 2);

  $effect(() => {
    if (!active) return;
    const q = sought;
    const ctrl = new AbortController();
    const timer = setTimeout(() => {
      find(q, ctrl.signal).then(
        (value) => {
          if (!ctrl.signal.aborted) last = { query: q, value };
        },
        (err) => {
          if (!ctrl.signal.aborted) last = { query: q, error: errMsg(err, 'unknown_error') };
        },
      );
    }, delay(q));
    return () => {
      clearTimeout(timer);
      ctrl.abort();
    };
  });

  return {
    get active() {
      return active;
    },
    get result() {
      return active && last?.query === sought ? last : null;
    },
  };
}
