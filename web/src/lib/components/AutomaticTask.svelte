<script lang="ts">
  // One task of the automatic-tasks window: its switch, and its settings while on.
  import type { Snippet } from 'svelte';

  let {
    title,
    hint,
    on = $bindable(),
    disabled,
    children,
  }: {
    title: string;
    hint: string;
    on: boolean;
    disabled: boolean;
    children: Snippet;
  } = $props();
</script>

<section class="flex flex-col gap-3 rounded-box bg-base-200 p-3">
  <label class="label cursor-pointer items-start gap-3">
    <input type="checkbox" class="checkbox checkbox-sm mt-0.5" bind:checked={on} {disabled} />
    <span class="whitespace-normal text-sm leading-snug">
      <span class="font-medium text-base-content">{title}</span>
      <span class="mt-0.5 block text-xs text-base-content/60">{hint}</span>
    </span>
  </label>
  {#if on}
    <div class="flex flex-col gap-3 pl-8 text-sm">{@render children()}</div>
  {/if}
</section>
