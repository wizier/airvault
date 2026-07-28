// Reusable reactive logic (Svelte runes in a .svelte.ts module). Thin hooks that
// wrap an $effect so components express timer lifecycle declaratively instead of
// hand-tracking a nullable handle and clearing it in a separate teardown.

/** Clear a transient value `ms` after it appears. The effect re-arms whenever
 *  the value changes and tears the timer down on unmount — no handle to track. */
export function autoDismiss(get: () => unknown, clear: () => void, ms = 15_000): void {
  $effect(() => {
    if (get() == null) return;
    const timer = setTimeout(clear, ms);
    return () => clearTimeout(timer);
  });
}
