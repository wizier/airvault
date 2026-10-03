<script lang="ts">
  // Which way a call went, red when missed.
  import type { BackupCall } from '../api/backup-contents';
  import Icon from './Icon.svelte';

  let { call, size }: { call: Pick<BackupCall, 'outgoing' | 'answered'>; size: number } = $props();

  const missed = $derived(!call.outgoing && !call.answered);
</script>

<Icon
  name={missed ? 'callMissed' : call.outgoing ? 'callOutgoing' : 'callIncoming'}
  {size}
  class={`shrink-0 ${missed ? 'text-error' : ''}`}
/>
