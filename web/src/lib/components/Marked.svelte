<script lang="ts">
  // Text with each place it says term marked, whatever the case, е and ё
  // alike; clipped, it starts a little before the first.
  let { text, term = '', clipped = false }: { text: string; term?: string; clipped?: boolean } = $props();

  // Plain and marked parts in turn, the first plain.
  const parts = $derived.by(() => {
    if (!term) return [text];
    const pattern = term.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/[её]/giu, '[её]');
    const parts = text.split(new RegExp(`(${pattern})`, 'giu'));
    const lead = [...parts[0]];
    if (clipped && parts.length > 1 && lead.length > 24) parts[0] = `…${lead.slice(-24).join('')}`;
    return parts;
  });
</script>

{#each parts as part, i (i)}{#if i % 2}<mark class="rounded-sm bg-warning/40 text-inherit">{part}</mark>{:else}{part}{/if}{/each}
