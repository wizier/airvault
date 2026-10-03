<script lang="ts">
  import { listBackupContacts, type BackupContact } from '../api/backups';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    onclose: () => void;
  } = $props();

  let expanded = $state<BackupContact | null>(null);

  function heading(c: BackupContact): string {
    return c.name || c.organization || 'No name';
  }

  function secondLine(c: BackupContact): string {
    return (c.name ? c.organization : '') || c.phones?.[0]?.value || c.emails?.[0]?.value || '';
  }
</script>

<SearchList
  {title}
  {subtitle}
  noun="contacts"
  placeholder="Search names, numbers, emails"
  load={(signal) => listBackupContacts(snapshotId, signal)}
  text={(c) => [c.name, c.organization, ...(c.phones ?? []).map((p) => p.value), ...(c.emails ?? []).map((e) => e.value)]}
  {onclose}
>
  {#snippet row(c)}
    <button type="button" class="list-col-grow min-w-0 text-left" onclick={() => (expanded = expanded === c ? null : c)}>
      <p class="truncate font-medium">{heading(c)}</p>
      <p class="truncate text-xs text-base-content/50">{secondLine(c)}</p>
    </button>
    {#if expanded === c}
      <dl class="list-col-wrap grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
        {#if c.jobTitle}
          <dt class="text-base-content/50">Job title</dt>
          <dd>{c.jobTitle}</dd>
        {/if}
        {#if c.name && c.organization}
          <dt class="text-base-content/50">Company</dt>
          <dd>{c.organization}</dd>
        {/if}
        {#each c.phones ?? [] as phone, j (j)}
          <dt class="text-base-content/50">{phone.label || 'Phone'}</dt>
          <dd class="select-all break-all">{phone.value}</dd>
        {/each}
        {#each c.emails ?? [] as email, j (j)}
          <dt class="text-base-content/50">{email.label || 'Email'}</dt>
          <dd class="select-all break-all">{email.value}</dd>
        {/each}
        {#if c.note}
          <dt class="text-base-content/50">Note</dt>
          <dd class="whitespace-pre-wrap wrap-anywhere">{c.note}</dd>
        {/if}
      </dl>
    {/if}
  {/snippet}
</SearchList>
