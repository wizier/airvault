<script lang="ts">
  // A menu of what the restore point holds; each view opens on top of it. A
  // backup the server holds locked asks for the password alone, and no password
  // closes it all. One locked later, for idleness, shows as a view's error.
  import { onMount } from 'svelte';
  import { backupGallerySource, listBackupComponents, type BackupComponent } from '../api/backup-contents';
  import type { RestorePoint } from '../api/backups';
  import { ApiError, errMsg } from '../api/client';
  import { formatBytes, formatDateTime } from '../format';
  import BackupCalls from './BackupCalls.svelte';
  import BackupContacts from './BackupContacts.svelte';
  import BackupMessages from './BackupMessages.svelte';
  import BackupNotes from './BackupNotes.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Gallery from './Gallery.svelte';
  import Icon from './Icon.svelte';
  import type { IconName } from './icons';
  import Modal from './Modal.svelte';
  import Pill from './Pill.svelte';
  import UnlockBackupModal from './UnlockBackupModal.svelte';

  let { point, deviceName, onclose }: {
    point: RestorePoint;
    deviceName: string;
    onclose: () => void;
  } = $props();

  // Tones echo the iPhone apps' icons.
  const COMPONENTS: Record<BackupComponent, { icon: IconName; tone: string; title: string; description: string }> = {
    photos: { icon: 'image', tone: 'bg-primary/10 text-primary', title: 'Photos', description: 'Photos, videos and Live Photos by month and album' },
    messages: { icon: 'messages', tone: 'bg-success/15 text-success', title: 'Messages', description: 'iMessage, SMS and RCS chats with attachments' },
    whatsapp: { icon: 'chat', tone: 'bg-success/15 text-success', title: 'WhatsApp', description: 'Chats, groups, photos, voice and documents' },
    notes: { icon: 'notes', tone: 'bg-warning/15 text-warning', title: 'Notes', description: 'Notes by folder; locked ones stay locked' },
    contacts: { icon: 'contacts', tone: 'bg-info/15 text-info', title: 'Contacts', description: 'Names, phone numbers and emails' },
    calls: { icon: 'call', tone: 'bg-accent/15 text-accent', title: 'Calls', description: 'Phone, FaceTime and app calls' },
  };

  let components = $state<BackupComponent[] | null>(null);
  let error = $state<string | null>(null);
  let locked = $state(false);
  let open = $state<BackupComponent | null>(null);
  const ctrl = new AbortController();
  const created = $derived(`Restore point from ${formatDateTime(point.created)}`);
  // The server answers at once whether an encrypted backup is unlocked: until
  // then nothing shows, so a locked one shows only the password prompt.
  const showMenu = $derived(!locked && (components !== null || error !== null || !point.encrypted));

  async function load(): Promise<void> {
    error = null;
    try {
      components = await listBackupComponents(point.snapshotId, ctrl.signal);
    } catch (err) {
      if (ctrl.signal.aborted) return;
      if (err instanceof ApiError && err.code === 'backup_locked') locked = true;
      else error = errMsg(err, 'backup_unlock_failed');
    }
  }

  onMount(() => {
    void load();
    return () => ctrl.abort();
  });
</script>

{#if locked}
  <UnlockBackupModal
    {point}
    onclose={(unlocked) => {
      if (!unlocked) return onclose();
      locked = false;
      void load();
    }}
  />
{:else if showMenu}
  <Modal
    title={`Backup of ${deviceName}`}
    subtitle={created}
    closable
    {onclose}
  >
    <div class="flex flex-wrap gap-1.5 pt-2">
      <Pill tone={point.encrypted ? 'green' : 'slate'}>{point.encrypted ? 'Encrypted' : 'Not encrypted'}</Pill>
      {#if point.iosVersion}<Pill>iOS {point.iosVersion}</Pill>{/if}
      <Pill>{formatBytes(point.sizeBytes)}</Pill>
    </div>
    <div class="py-3">
      {#if error}
        <ErrorLine {error} variant="alert" />
        <button type="button" class="btn btn-ghost btn-sm mt-2" onclick={load}>Try again</button>
      {:else if components === null}
        <p class="flex items-center gap-2 text-sm text-base-content/60">
          <span class="loading loading-spinner loading-sm"></span>
          Opening the backup…
        </p>
      {:else if components.length === 0}
        <p class="text-sm text-base-content/60">This backup holds nothing AirVault can show yet.</p>
      {:else}
        <div class="grid grid-cols-2 gap-3">
          {#each components as component (component)}
            {@const view = COMPONENTS[component]}
            <button
              type="button"
              class="card card-border card-sm bg-base-100 text-left transition hover:border-primary/40 hover:shadow-md"
              onclick={() => (open = component)}
            >
              <div class="card-body">
                <span class={`flex h-11 w-11 items-center justify-center rounded-box ${view.tone}`}>
                  <Icon name={view.icon} size={22} />
                </span>
                <h3 class="card-title mt-1 text-base">{view.title}</h3>
                <p class="text-xs text-base-content/60">{view.description}</p>
              </div>
            </button>
          {/each}
        </div>
      {/if}
    </div>
  </Modal>
{/if}

{#if open}
  {@const view = {
    title: `${COMPONENTS[open].title} — backup of ${deviceName}`,
    subtitle: created,
    onclose: () => (open = null),
  }}
  {@const snapshotId = point.snapshotId}
  {#if open === 'photos'}
    <Gallery source={backupGallerySource(snapshotId)} rescan={false} {...view} />
  {:else if open === 'messages' || open === 'whatsapp'}
    <BackupMessages {snapshotId} app={open} {...view} />
  {:else if open === 'notes'}
    <BackupNotes {snapshotId} {...view} />
  {:else if open === 'contacts'}
    <BackupContacts {snapshotId} {...view} />
  {:else if open === 'calls'}
    <BackupCalls {snapshotId} {...view} />
  {/if}
{/if}
