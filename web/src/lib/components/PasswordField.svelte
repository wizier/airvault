<script lang="ts">
  import type { HTMLInputAttributes } from 'svelte/elements';

  let {
    label,
    value = $bindable(''),
    placeholder = '',
    autocomplete = 'off',
    disabled = false,
    error = null,
    focus = false,
    className = '',
  }: {
    label: string;
    value?: string;
    placeholder?: string;
    autocomplete?: HTMLInputAttributes['autocomplete'];
    disabled?: boolean;
    error?: string | null;
    focus?: boolean;
    className?: string;
  } = $props();

  let input = $state<HTMLInputElement>();

  $effect(() => {
    if (focus) input?.focus();
  });
</script>

<label class={`${className} flex flex-col gap-1.5`}>
  <span class="label">{label}</span>
  <input
    type="password"
    class={`input w-full ${error ? 'input-error' : ''}`}
    bind:value
    bind:this={input}
    {placeholder}
    {autocomplete}
    {disabled}
    aria-invalid={error !== null}
  />
  {#if error}
    <span class="text-xs text-error">{error}</span>
  {/if}
</label>
