<script lang="ts">
  import { backupAppIconUrl, listBackupPasswords, type BackupSecret } from '../api/backup-contents';
  import { autoDismiss } from '../timers.svelte';
  import type { IconName } from './icons';
  import Icon from './Icon.svelte';
  import SearchList from './SearchList.svelte';

  let { title, subtitle, snapshotId, onclose }: {
    title: string;
    subtitle: string;
    snapshotId: string;
    onclose: () => void;
  } = $props();

  // Passwords stay masked until asked for, keyed by the secret's id; a fresh
  // open remounts, so nothing carries over.
  let shown = $state<Record<number, boolean>>({});
  // Which field was just copied, as "<field>:<id>", so one row's two buttons
  // confirm apart.
  let copied = $state<string | null>(null);
  autoDismiss(() => copied, () => (copied = null), 1500);

  const KIND: Record<BackupSecret['kind'], { icon: IconName; tone: string }> = {
    wifi: { icon: 'wifi', tone: 'text-primary' },
    app: { icon: 'key', tone: 'text-base-content/60' },
    web: { icon: 'globe', tone: 'text-info' },
  };

  const AUTH: Record<string, string> = { form: 'web form', basic: 'Basic auth', digest: 'Digest auth' };

  // Where a password is used and what saved it, such as "HTTPS · Basic auth ·
  // Safari"; an app named in the title or its group's head already is left out.
  function details(s: BackupSecret, grouped: boolean): string {
    const auth = s.auth ? (AUTH[s.auth] ?? s.auth) : '';
    const app = grouped || s.app === s.title ? '' : s.app;
    return [s.protocol?.toUpperCase(), s.port ? `port ${s.port}` : '', auth, app]
      .filter(Boolean)
      .join(' · ');
  }

  // The Clipboard API is for secure origins only; over plain HTTP, as AirVault
  // on a LAN often is, the older copy command still works. Its textarea goes
  // beside the button: the open modal leaves the rest of the page inert.
  function writeClipboard(text: string, button: HTMLElement): Promise<void> {
    if (navigator.clipboard) return navigator.clipboard.writeText(text);
    const area = document.createElement('textarea');
    area.value = text;
    area.style.cssText = 'position: fixed; opacity: 0';
    button.after(area);
    area.select();
    const done = document.execCommand('copy');
    area.remove();
    button.focus();
    return done ? Promise.resolve() : Promise.reject(new Error('copy failed'));
  }

  function copy(key: string, text: string, button: HTMLElement): void {
    writeClipboard(text, button).then(() => (copied = key), () => {}); // denied: the eye still shows it
  }
</script>

{#snippet badge(kind: BackupSecret['kind'], bundleId?: string)}
  <span class="flex size-6 shrink-0 items-center justify-center">
    {#if bundleId}
      <img src={backupAppIconUrl(snapshotId, bundleId)} alt="" loading="lazy" class="size-6 rounded-[22%]" />
    {:else}
      <Icon name={KIND[kind].icon} size={18} class={KIND[kind].tone} />
    {/if}
  </span>
{/snippet}

{#snippet copyButton(s: BackupSecret, field: 'username' | 'password', text: string, cls: string, size: number)}
  {@const key = `${field}:${s.id}`}
  <button
    type="button"
    class={cls}
    title={`Copy ${field}`}
    aria-label={`Copy ${s.title} ${field}`}
    onclick={(event) => copy(key, text, event.currentTarget)}
  >
    <Icon name={copied === key ? 'check' : 'copy'} {size} class={copied === key ? 'text-success' : ''} />
  </button>
{/snippet}

{#snippet appHead(items: BackupSecret[])}
  {@render badge(items[0].kind, items[0].bundleId)}
  <span class="min-w-0">
    <span class="block truncate font-medium">{items[0].app}</span>
    <span class="block text-xs text-base-content/50">{items.length} passwords</span>
  </span>
{/snippet}

<SearchList
  {title}
  {subtitle}
  noun="passwords"
  placeholder="Search networks, apps and sites"
  load={(signal) => listBackupPasswords(snapshotId, signal)}
  text={(s) => [s.title, s.account, s.app]}
  chips={[
    { label: 'All', shows: () => true },
    { label: 'Wi-Fi', shows: (s) => s.kind === 'wifi' },
    { label: 'Apps', shows: (s) => s.kind === 'app' },
    { label: 'Sites', shows: (s) => s.kind === 'web' },
  ]}
  group={{
    // Safari's are the sites the Passwords app lists: they stay one list.
    key: (s) => (s.kind !== 'wifi' && s.app !== 'Safari' ? s.app : undefined),
    head: appHead,
  }}
  {onclose}
>
  {#snippet row(s, grouped)}
    {@const info = details(s, grouped)}
    {@render badge(s.kind, grouped ? undefined : s.bundleId)}
    <div class="list-col-grow min-w-0">
      <p class="truncate font-medium">{s.title || 'Unnamed'}</p>
      {#if info}<p class="truncate text-xs text-base-content/50">{info}</p>{/if}
      {#if s.account}
        <p class="flex items-center gap-1 text-xs text-base-content/50">
          <span class="min-w-0 truncate">{s.account}</span>
          {@render copyButton(s, 'username', s.account, 'shrink-0 opacity-60 hover:opacity-100', 12)}
        </p>
      {/if}
      <p class="mt-0.5 font-mono text-sm break-all">
        {#if shown[s.id]}{s.password}{:else}••••••••{/if}
      </p>
    </div>
    <div class="flex shrink-0 items-center gap-1">
      <button
        type="button"
        class="btn btn-square btn-ghost btn-xs"
        title={shown[s.id] ? 'Hide' : 'Show'}
        aria-label={shown[s.id] ? 'Hide password' : 'Show password'}
        onclick={() => (shown[s.id] = !shown[s.id])}
      >
        <Icon name={shown[s.id] ? 'eyeOff' : 'eye'} size={15} />
      </button>
      {@render copyButton(s, 'password', s.password, 'btn btn-square btn-ghost btn-xs', 15)}
    </div>
  {/snippet}
</SearchList>
