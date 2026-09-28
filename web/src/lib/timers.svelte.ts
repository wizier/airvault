/** Clears a transient value `ms` after it appears; re-arms whenever it changes. */
export function autoDismiss(get: () => unknown, clear: () => void, ms = 15_000): void {
  $effect(() => {
    if (get() == null) return;
    const timer = setTimeout(clear, ms);
    return () => clearTimeout(timer);
  });
}
