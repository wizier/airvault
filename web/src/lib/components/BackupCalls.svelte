<script lang="ts">
  import { listBackupCalls, type BackupCall } from '../api/backup-contents';
  import { formatDateTime, formatSeconds } from '../format';
  import Icon from './Icon.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    onclose: () => void;
  } = $props();

  // Apps that call through CallKit record their bundle ID.
  const SERVICES: Record<string, string> = {
    phone: 'Phone',
    facetime: 'FaceTime',
    'net.whatsapp.WhatsApp': 'WhatsApp',
    'ph.telegra.Telegraph': 'Telegram',
    'com.viber': 'Viber',
    'org.whispersystems.signal': 'Signal',
  };

  const missed = (c: BackupCall) => !c.outgoing && !c.answered;

  function details(c: BackupCall): string {
    const service = `${SERVICES[c.service] ?? c.service}${c.video ? ' Video' : ''}`;
    return [c.name ? c.address : '', service, c.duration ? formatSeconds(c.duration) : '']
      .filter(Boolean)
      .join(' · ');
  }
</script>

<SearchList
  {title}
  {subtitle}
  noun="calls"
  placeholder="Search names and numbers"
  load={(signal) => listBackupCalls(snapshotId, signal)}
  text={(c) => [c.name, c.address]}
  chips={[
    { label: 'All', shows: () => true },
    { label: 'Missed', shows: missed },
  ]}
  {onclose}
>
  {#snippet row(c)}
    <Icon
      name={missed(c) ? 'callMissed' : c.outgoing ? 'callOutgoing' : 'callIncoming'}
      size={16}
      class={missed(c) ? 'text-error' : 'text-base-content/50'}
    />
    <div class="list-col-grow min-w-0">
      <p class={`truncate font-medium ${missed(c) ? 'text-error' : ''}`}>{c.name || c.address || 'No Caller ID'}</p>
      <p class="truncate text-xs text-base-content/50">{details(c)}</p>
    </div>
    <span class="text-xs text-base-content/50">{formatDateTime(c.time)}</span>
  {/snippet}
</SearchList>
