<script lang="ts">
  import { ICONS, type IconName } from './icons';

  let {
    name,
    size = 18,
    class: cls = '',
    stroke = 1.75,
  }: {
    name: IconName;
    size?: number;
    class?: string;
    stroke?: number;
  } = $props();

  const Cmp = $derived(ICONS[name]);
</script>

<Cmp {size} strokeWidth={stroke} class={cls} aria-hidden="true" />
