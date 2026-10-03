<script lang="ts">
  // Open while mounted: any way of closing it calls onclose, and the host
  // unmounts it, so per-open state resets by remount. `locked` holds it open
  // while a request that must not be abandoned runs.
  import type { Snippet } from 'svelte';
  import Icon from './Icon.svelte';

  // Beyond daisyUI's own dialog, a column: medium grows with what it shows,
  // tall stays as high while a list loads and scrolls, wide is for a chat,
  // photos or a log.
  const SIZES = {
    medium: 'flex max-h-[85vh] max-w-2xl flex-col',
    tall: 'flex h-[85vh] max-w-2xl flex-col',
    wide: 'flex h-[90vh] w-11/12 max-w-5xl flex-col',
  };

  let {
    title,
    heading,
    leading,
    subtitle,
    closable = false,
    locked = false,
    size,
    class: cls = '',
    onclose,
    headerActions,
    children,
    actions,
  }: {
    title?: string;
    /** Replaces the plain title text. */
    heading?: Snippet;
    /** Before the title and the subtitle, such as a picture. */
    leading?: Snippet;
    subtitle?: string;
    /** A ✕ in the header, for windows without a Cancel button. */
    closable?: boolean;
    locked?: boolean;
    size?: keyof typeof SIZES;
    /** The layout of the box. */
    class?: string;
    onclose: () => void;
    headerActions?: Snippet;
    children: Snippet;
    /** The footer buttons. */
    actions?: Snippet;
  } = $props();

  let dialog: HTMLDialogElement;

  export function close(): void {
    dialog.close();
  }
</script>

<dialog
  class="modal"
  bind:this={dialog}
  {@attach (d) => d.showModal()}
  oncancel={(event) => locked && event.preventDefault()}
  {onclose}
>
  <div class={`modal-box ${size ? SIZES[size] : ''} ${cls}`}>
    {#if title || heading}
      <div class="flex shrink-0 items-start justify-between gap-3">
        <div class="flex min-w-0 items-center gap-3">
          {@render leading?.()}
          <div class="min-w-0">
            <h3 class="flex items-center gap-2 text-lg font-bold">
              {#if heading}{@render heading()}{:else}{title}{/if}
            </h3>
            {#if subtitle}
              <p class="mt-0.5 text-sm text-base-content/60">{subtitle}</p>
            {/if}
          </div>
        </div>
        {#if closable || headerActions}
          <div class="flex shrink-0 items-center gap-1">
            {@render headerActions?.()}
            {#if closable}
              <button type="button" class="btn btn-square btn-ghost btn-sm" aria-label="Close" disabled={locked} onclick={close}>
                <Icon name="x" size={16} />
              </button>
            {/if}
          </div>
        {/if}
      </div>
    {/if}
    {@render children()}
    {#if actions}
      <div class="modal-action shrink-0">{@render actions()}</div>
    {/if}
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close" disabled={locked}>close</button>
  </form>
</dialog>
