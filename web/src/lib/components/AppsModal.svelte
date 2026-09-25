<script lang="ts">
  // Installed applications: fetched fresh per open, client-side search, USER
  // apps only. Icons load in batches for rows near the viewport.
  // Per row: browse Documents (file-sharing apps) and uninstall; the toolbar
  // installs an .ipa with live percent streamed in the install response.
  import { onMount, untrack } from 'svelte';
  import { appIcons, cachedAppIcons, installApp, uninstallApp, type DeviceApp, type InstallProgress } from '../api/apps';
  import { errMsg } from '../api/client';
  import { appFileSource } from '../api/files';
  import { createBatchLoader, nearViewport } from '../batch-loader.svelte';
  import { liveRun } from '../events.svelte';
  import { deviceAppsResources } from '../stores.svelte';
  import FileBrowser from './FileBrowser.svelte';
  import ErrorLine from './ErrorLine.svelte';
  import Icon from './Icon.svelte';

  let { udid, name, onclose }: { udid: string; name: string; onclose: () => void } = $props();

  let dialog: HTMLDialogElement;
  let search = $state('');
  const appsResource = $derived(deviceAppsResources.for(udid));
  const live = $derived(liveRun(udid));
  const runActive = $derived(live !== null);
  const restoreRunning = $derived(live?.restore ?? false);
  const apps = $derived(appsResource.data ?? []);
  const error = $derived(appsResource.loadError('app_list_failed'));
  const loading = $derived(!appsResource.ready && !error);

  let installing = $state(false);
  let uploadPct = $state(0);
  let installState = $state<InstallProgress | null>(null);
  let note = $state<{ tone: 'ok' | 'error'; text: string } | null>(null);

  // Uninstall: an inline two-step confirm on the row.
  let confirmUninstall = $state<string | null>(null);
  let uninstalling = $state<string | null>(null);
  const appMutationActive = $derived(installing || uninstalling !== null);
  const writeBusy = $derived(runActive || appMutationActive);
  const readBusy = $derived(restoreRunning || appMutationActive);

  // Documents browser (opens over this modal for a file-sharing app).
  let filesApp = $state<DeviceApp | null>(null);
  const filesSource = $derived(
    filesApp ? appFileSource(udid, filesApp.bundleId, !writeBusy) : null,
  );

  const installPct = $derived(installState?.percent ?? uploadPct);
  // Before the phone's first progress line a finished upload is already on its
  // way to the phone; unknown future phases read as 'Installing', never as 'Uploading'.
  const installStage = $derived(
    installState
      ? installState.phase === 'staging'
        ? 'Sending to iPhone'
        : 'Installing'
      : uploadPct >= 100
        ? 'Sending to iPhone'
        : 'Uploading',
  );
  const installIndeterminate = $derived(!installState && uploadPct >= 100);

  async function handleInstall(e: Event) {
    const input = e.currentTarget as HTMLInputElement;
    const file = input.files?.[0];
    input.value = ''; // let the same file be re-picked later
    if (!file || writeBusy) return;
    uploadPct = 0;
    installState = null;
    installing = true;
    note = null;
    try {
      await installApp(
        udid,
        file,
        (percent) => (uploadPct = percent),
        (progress) => (installState = progress),
      );
      note = { text: `Installed ${file.name}`, tone: 'ok' };
    } catch (err) {
      note = { text: errMsg(err, 'app_install_failed'), tone: 'error' };
    } finally {
      installing = false;
      installState = null;
    }
  }

  async function doUninstall(bundleId: string) {
    uninstalling = bundleId;
    note = null;
    try {
      await uninstallApp(udid, bundleId);
      appsResource.mutate((current) => current.filter((app) => app.bundleId !== bundleId));
      confirmUninstall = null;
    } catch (err) {
      note = { text: errMsg(err, 'app_uninstall_failed'), tone: 'error' };
    } finally {
      uninstalling = null;
    }
  }

  // Each open subscribes/refetches; app.catalog invalidates this same resource
  // after installs and removals, regardless of which UI initiated them.
  $effect(() => appsResource.start());

  const visible = $derived.by(() => {
    const needle = search.toLowerCase();
    if (!needle) return apps;
    return apps.filter(
      (a) => a.name.toLowerCase().includes(needle) || a.bundleId.toLowerCase().includes(needle),
    );
  });

  // PNG data URLs by bundle id; '' marks an app the phone has no icon for.
  let icons = $state<Record<string, string>>(untrack(() => ({ ...cachedAppIcons(udid) })));
  const iconLoader = createBatchLoader<string>({
    batchSize: 30,
    debounceMs: 120,
    fetchBatch: (bundleIds, signal) => appIcons(udid, bundleIds, signal),
    onBatch: (bundleIds, urls) => {
      for (const bundleId of bundleIds) icons[bundleId] = urls[bundleId] ?? '';
    },
  });

  // Rows coming near the viewport queue their icon.
  const rows = nearViewport('600px 0px', (bundleId, near) => {
    if (near && icons[bundleId] === undefined) iconLoader.queue(bundleId);
  });

  // Closing the modal cancels icon batches still in flight.
  onMount(() => () => iconLoader.reset());
</script>

<dialog class="modal" bind:this={dialog} {@attach (d) => d.showModal()} {onclose}>
  <div class="modal-box flex h-[85vh] max-w-2xl flex-col gap-3 overflow-hidden">
    <div class="flex shrink-0 flex-col gap-3">
      <div class="flex items-start justify-between gap-3">
        <div>
          <h3 class="text-lg font-bold">Apps</h3>
          <p class="mt-0.5 text-sm text-base-content/60">Apps installed on {name}</p>
        </div>
        <button type="button" class="btn btn-square btn-ghost btn-sm" aria-label="Close" onclick={() => dialog.close()}>
          <Icon name="x" size={16} />
        </button>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <label class="input input-sm flex-1">
          <input type="text" placeholder="Search name or bundle id…" bind:value={search} />
        </label>
        {#if !loading && !error}
          <span class="text-xs text-base-content/50">{visible.length} apps</span>
        {/if}
        <label
          class={`btn btn-outline btn-sm ${writeBusy ? 'pointer-events-none btn-disabled' : ''}`}
          title={writeBusy ? 'The device is busy' : 'Install a user-provided .ipa'}
        >
          {#if installing}
            <span class="loading loading-spinner loading-xs"></span>
            Installing
          {:else}
            <Icon name="upload" size={14} /> Install .ipa…
          {/if}
          <input type="file" accept=".ipa" class="hidden" disabled={writeBusy} onchange={handleInstall} />
        </label>
      </div>
      {#if installing}
        <div class="flex flex-col gap-1.5">
          <div class="flex items-center justify-between text-sm text-base-content/70">
            <span>{installStage}</span>
            {#if !installIndeterminate}
              <span class="font-mono tabular-nums">{installPct}%</span>
            {/if}
          </div>
          {#if installIndeterminate}
            <progress
              class="progress progress-primary w-full"
              aria-label={`${installStage} application`}
            ></progress>
          {:else}
            <progress
              class="progress progress-primary w-full"
              aria-label={`${installStage} application: ${installPct}%`}
              value={installPct}
              max="100"
            ></progress>
          {/if}
        </div>
      {/if}
      {#if note}
        <div
          role={note.tone === 'error' ? 'alert' : 'status'}
          class={`alert py-2 ${note.tone === 'error' ? 'alert-error alert-soft' : 'alert-success alert-soft'}`}
        >
          <Icon name={note.tone === 'error' ? 'alert' : 'check'} size={13} stroke={2} />
          <span class="text-sm">{note.text}</span>
        </div>
      {/if}
    </div>

    <div class="min-h-0 flex-1 overflow-auto rounded-box bg-base-200" {@attach rows.root}>
      {#if loading}
        <p class="flex items-center gap-2 p-4 text-sm text-base-content/60">
          <span class="loading loading-spinner loading-sm"></span>
          Asking the phone…
        </p>
      {:else if error}
        <ErrorLine {error} variant="alert" className="m-3" />
      {:else if visible.length === 0}
        <p class="p-4 text-sm text-base-content/50">
          {search ? 'Nothing matches the search' : 'No apps reported'}
        </p>
      {:else}
        <ul class="divide-y divide-base-300/60">
          {#each visible as app (app.bundleId)}
            <li class="flex items-center gap-3 px-4 py-2" {@attach rows.item(app.bundleId)}>
              {#if icons[app.bundleId]}
                <img
                  src={icons[app.bundleId]}
                  alt=""
                  class="h-9 w-9 shrink-0 rounded-[22%] bg-base-300/60 object-cover"
                  onerror={(e) => ((e.currentTarget as HTMLImageElement).style.visibility = 'hidden')}
                />
              {:else}
                <!-- Grey tile while loading; an app without an icon keeps an empty slot. -->
                <span
                  class={`h-9 w-9 shrink-0 rounded-[22%] ${icons[app.bundleId] === undefined ? 'bg-base-300/60' : ''}`}
                ></span>
              {/if}
              <div class="min-w-0 flex-1">
                <p class="truncate text-sm font-medium">{app.name}</p>
                <p class="truncate font-mono text-xs text-base-content/50">{app.bundleId}</p>
              </div>
              {#if app.version}
                <span class="shrink-0 font-mono text-xs text-base-content/60">{app.version}</span>
              {/if}
              <div class="flex shrink-0 items-center gap-1">
                {#if app.fileSharing}
                  <button
                    type="button"
                    class="btn btn-square btn-ghost btn-xs"
                    disabled={readBusy}
                    title={readBusy ? 'The device is busy' : 'Browse files'}
                    aria-label={`Browse ${app.name} files`}
                    onclick={() => (filesApp = app)}
                  >
                    <Icon name="folder" size={15} />
                  </button>
                {/if}
                {#if confirmUninstall === app.bundleId}
                  <button
                    type="button"
                    class="btn btn-error btn-xs"
                    disabled={writeBusy}
                    onclick={() => doUninstall(app.bundleId)}
                  >
                    {#if uninstalling === app.bundleId}
                      <span class="loading loading-spinner loading-xs"></span>
                    {:else}
                      Remove
                    {/if}
                  </button>
                  <button type="button" class="btn btn-ghost btn-xs" disabled={uninstalling !== null} onclick={() => (confirmUninstall = null)}>
                    Cancel
                  </button>
                {:else}
                  <button
                    type="button"
                    class="btn btn-square btn-ghost btn-xs"
                    disabled={writeBusy}
                    title={writeBusy ? 'The device is busy' : 'Uninstall'}
                    aria-label={`Uninstall ${app.name}`}
                    onclick={() => (confirmUninstall = app.bundleId)}
                  >
                    <Icon name="trash" size={15} />
                  </button>
                {/if}
              </div>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  </div>
  <form method="dialog" class="modal-backdrop">
    <button aria-label="Close">close</button>
  </form>
</dialog>

{#if filesApp && filesSource}
  <FileBrowser
    source={filesSource}
    title={`Files — ${filesApp.name}`}
    subtitle="The app's Documents folder on the device"
    rootLabel="Documents"
    onclose={() => (filesApp = null)}
  />
{/if}
