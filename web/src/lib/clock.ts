// Reactive "now" for relative timestamps: one shared, ref-counted interval
// (readable's start runs at the first subscriber, cleanup at the last). A tick
// re-renders every $now label at once; a tab regaining focus catches up.

import { readable } from 'svelte/store';

export const now = readable(Date.now(), (set) => {
  set(Date.now()); // fresh value on (re)subscribe, before the first tick
  const id = setInterval(() => set(Date.now()), 30_000);
  const onVisible = () => {
    if (!document.hidden) set(Date.now());
  };
  document.addEventListener('visibilitychange', onVisible);
  return () => {
    clearInterval(id);
    document.removeEventListener('visibilitychange', onVisible);
  };
});
