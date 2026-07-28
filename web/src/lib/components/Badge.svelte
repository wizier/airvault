<script lang="ts">
  import Icon from './Icon.svelte';
  import type { IconName } from './icons';

  // A small on/off capability chip: `on` drives the accent styling; off reads
  // as a muted ghost badge with an X.
  let {
    label,
    icon,
    on = false,
    offLabel,
  }: {
    label: string;
    icon: IconName;
    on?: boolean;
    offLabel?: string;
  } = $props();
</script>

<span
  title={on ? label : (offLabel ?? `${label}: off`)}
  class={`badge badge-sm gap-1 ${on ? 'badge-soft badge-primary' : 'badge-ghost'}`}
>
  <Icon name={on ? icon : 'x'} size={12} stroke={2} />
  {on ? label : (offLabel ?? label)}
</span>
