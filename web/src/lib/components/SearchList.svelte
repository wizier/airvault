<script lang="ts" generics="T">
  // A list read from a backup at once, narrowed by chips and a search over each
  // item's text; the caller draws the rows.
  import { onMount, type Snippet } from 'svelte';
  import { errMsg } from '../api/client';
  import ErrorLine from './ErrorLine.svelte';
  import Modal from './Modal.svelte';

  let {
    title,
    subtitle,
    noun,
    placeholder,
    load,
    text,
    chips = [],
    row,
    onclose,
  }: {
    title: string;
    subtitle: string;
    /** "contacts": the subtitle counts them once loaded. */
    noun: string;
    placeholder: string;
    load: (signal: AbortSignal) => Promise<T[]>;
    /** What the search looks in. */
    text: (item: T) => (string | undefined)[];
    /** Views of the list; the first is the default. */
    chips?: { label: string; shows: (item: T) => boolean }[];
    row: Snippet<[T]>;
    onclose: () => void;
  } = $props();

  let items = $state.raw<T[] | null>(null);
  let error = $state<string | null>(null);
  let query = $state('');
  let chip = $state(0);
  const chipGroup = $props.id();

  const shown = $derived.by(() => {
    const q = query.trim().toLowerCase();
    const shows = chips[chip]?.shows ?? (() => true);
    return (items ?? []).filter(
      (item) => shows(item) && (!q || text(item).some((field) => field?.toLowerCase().includes(q))),
    );
  });

  onMount(() => {
    const ctrl = new AbortController();
    load(ctrl.signal)
      .then((list) => (items = list))
      .catch((err) => {
        if (!ctrl.signal.aborted) error = errMsg(err, 'unknown_error');
      });
    return () => ctrl.abort();
  });
</script>

<Modal
  {title}
  subtitle={items ? `${items.length.toLocaleString()} ${noun}` : subtitle}
  closable
  size="tall"
  class="gap-3"
  {onclose}
>
  <div class="flex shrink-0 items-center gap-1">
    {#each chips as c, i (c.label)}
      <input type="radio" class="btn btn-xs" name={chipGroup} aria-label={c.label} value={i} bind:group={chip} />
    {/each}
    <input type="search" class="input input-sm min-w-0 flex-1" {placeholder} bind:value={query} />
  </div>

  <div class="min-h-0 flex-1 overflow-auto rounded-box bg-base-200">
    {#if error}
      <ErrorLine {error} variant="alert" className="m-3" />
    {:else if items === null}
      <p class="flex items-center gap-2 p-4 text-sm text-base-content/60">
        <span class="loading loading-spinner loading-sm"></span>
        Reading the backup…
      </p>
    {:else if shown.length === 0}
      <p class="p-4 text-sm text-base-content/50">{items.length ? 'Nothing matches' : `No ${noun} in this backup`}</p>
    {:else}
      <!-- Each row's children are list-row columns: list-col-grow takes the width, list-col-wrap a line below. -->
      <ul class="list">
        {#each shown as item (item)}
          <li class="list-row items-center py-2.5">{@render row(item)}</li>
        {/each}
      </ul>
    {/if}
  </div>
</Modal>
