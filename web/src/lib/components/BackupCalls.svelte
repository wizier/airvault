<script lang="ts">
  import { listBackupCalls, pictureUrl, type BackupCall } from '../api/backup-contents';
  import { formatDateTime, formatSeconds } from '../format';
  import Avatar from './Avatar.svelte';
  import CallIcon from './CallIcon.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    onclose: () => void;
  } = $props();

  // Other calls are an app's: its name, or its bundle ID once removed.
  const SERVICES: Record<string, string> = { phone: 'Phone', facetime: 'FaceTime' };

  const missed = (c: BackupCall) => !c.outgoing && !c.answered;

  function details(c: BackupCall): string {
    const service = `${SERVICES[c.service] ?? c.app ?? c.service}${c.video ? ' Video' : ''}`;
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
    <Avatar src={pictureUrl(snapshotId, 'calls', c)} name={c.name ?? ''} />
    <div class="list-col-grow min-w-0">
      <p class={`truncate font-medium ${missed(c) ? 'text-error' : ''}`}>{c.name || c.address || 'No Caller ID'}</p>
      <p class="flex items-center gap-1 text-xs text-base-content/50">
        <CallIcon call={c} size={12} />
        <span class="truncate">{details(c)}</span>
      </p>
    </div>
    <span class="text-xs text-base-content/50">{formatDateTime(c.time)}</span>
  {/snippet}
</SearchList>
