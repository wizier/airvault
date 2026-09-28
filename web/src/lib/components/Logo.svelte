<script lang="ts">
  let { size = 30 }: { size?: number } = $props();
</script>

<img
  src="/airvault-logo.svg"
  alt=""
  width={size}
  height={size}
  aria-hidden="true"
  draggable="false"
  decoding="async"
  class="block shrink-0"
/>
