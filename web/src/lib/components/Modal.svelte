<script lang="ts">
  // Open while mounted: any way of closing it calls onclose, and the host
  // unmounts it, so per-open state resets by remount. `locked` holds it open
  // while a request that must not be abandoned runs.
  import type { Snippet } from 'svelte';
  import Icon from './Icon.svelte';

  let {
    title,
    heading,
    subtitle,
    closable = false,
    locked = false,
    class: cls = '',
    onclose,
    headerActions,
    children,
    actions,
  }: {
    title?: string;
    /** Replaces the plain title text. */
    heading?: Snippet;
    subtitle?: string;
    /** A ✕ in the header, for windows without a Cancel button. */
    closable?: boolean;
    locked?: boolean;
    /** Sizing and layout of the box. */
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
  <div class={`modal-box ${cls}`}>
    {#if title || heading}
      <div class="flex shrink-0 items-start justify-between gap-3">
        <div class="min-w-0">
          <h3 class="flex items-center gap-2 text-lg font-bold">
            {#if heading}{@render heading()}{:else}{title}{/if}
          </h3>
          {#if subtitle}
            <p class="mt-0.5 text-sm text-base-content/60">{subtitle}</p>
          {/if}
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
