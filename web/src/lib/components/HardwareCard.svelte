<script lang="ts">
  import { errMsg } from '../api/client';
  import { hardwareResources } from '../stores.svelte';
  import { formatBytes } from '../format';
  import Icon from './Icon.svelte';

  let { udid, reachable }: { udid: string; reachable: boolean } = $props();

  const res = $derived(hardwareResources.for(udid));
  $effect(() => {
    if (reachable) return res.start();
  });
  const hw = $derived(res.data);
  const loading = $derived(res.loading);
  const error = $derived(res.error ? errMsg(res.error, 'hardware_query_failed') : null);

  const diskUsed = $derived(
    hw?.diskDataCapacity && hw.diskDataAvailable !== undefined
      ? hw.diskDataCapacity - hw.diskDataAvailable
      : null,
  );
  const diskPct = $derived(
    diskUsed !== null && hw?.diskDataCapacity ? Math.round((diskUsed / hw.diskDataCapacity) * 100) : null,
  );
  type Row = { label: string; value: string; mono?: boolean; secret?: boolean; cls?: string };

  const batteryRows = $derived.by(() => {
    const rows: Row[] = [];
    if (!hw) return rows;
    const push = (label: string, value: string, cls = '') => rows.push({ label, value, cls: `tabular-nums ${cls}` });
    const health = hw.batteryHealthPct;
    if (health) push('Health', `${health}%`, `font-medium ${health <= 79 ? 'text-warning' : ''}`);
    if (hw.batteryCycles) push('Cycles', `${hw.batteryCycles}`);
    if (hw.batteryMaxCapacity) push('Capacity', `${hw.batteryMaxCapacity} mAh`);
    if (hw.batteryTemperature) push('Temp', `${(hw.batteryTemperature / 100).toFixed(1)} °C`);
    if (hw.batteryVoltageMv) push('Voltage', `${(hw.batteryVoltageMv / 1000).toFixed(2)} V`);
    // Charge/discharge power in watts (P = V·I) — more meaningful than raw mA.
    // Positive = charging into the cell, negative = discharging.
    const watts = hw.batteryAmperageMa && hw.batteryVoltageMv
      ? (hw.batteryVoltageMv / 1000) * (hw.batteryAmperageMa / 1000)
      : 0;
    if (Math.abs(watts) >= 0.05) {
      push(watts > 0 ? 'Charging' : 'Draw', `${watts > 0 ? '+' : '−'}${Math.abs(watts).toFixed(1)} W`, watts > 0 ? 'text-success' : '');
    }
    return rows;
  });

  const idRows = $derived.by(() => {
    const rows: Row[] = [];
    if (!hw) return rows;
    const push = (label: string, value: string | undefined, mono = false, secret = false) => {
      if (value) rows.push({ label, value, mono, secret });
    };
    push('Model ID', hw.productType, true);
    push('Model', hw.modelNumber, true);
    push('Board', hw.hardwareModel, true);
    push('Region', hw.region, true);
    push('OS build', hw.buildVersion, true);
    push('Serial', hw.serial, true, true);
    push('Phone number', hw.phoneNumber, true, true);
    push('Time zone', hw.timeZone);
    push('Wi-Fi MAC', hw.wifiMac, true, true);
    push('Bluetooth MAC', hw.bluetoothMac, true, true);
    // A single SIM flows into the same identity grid so Carrier/IMEI line up
    // with the columns above; two+ SIMs render as titled sub-blocks instead.
    if ((hw.sims?.length ?? 0) === 1) {
      push('Carrier', hw.sims![0].carrier);
      push('IMEI', hw.sims![0].imei, true, true);
    }
    return rows;
  });

  const sims = $derived(hw?.sims ?? []);

  // Masked until revealed so shoulder-surfing or screenshots don't leak the
  // IMEI or phone number.
  let revealed = $state(false);
</script>

<div class="card bg-base-100 shadow-sm">
  <div class="card-body gap-4 p-5">
    <div class="flex items-center justify-between gap-3">
      <h2 class="text-sm font-semibold">Hardware &amp; storage</h2>
      <button
        type="button"
        class="btn btn-outline btn-sm"
        disabled={!reachable || loading}
        onclick={() => res.refresh()}
        title={!reachable ? 'Device is offline' : 'Re-read from the device'}
      >
        {#if loading}
          <span class="loading loading-spinner loading-xs"></span>
        {:else}
          <Icon name="refresh" size={13} />
        {/if}
        Refresh
      </button>
    </div>

    {#if error}
      <div class="flex items-center justify-between gap-3">
        <span class="flex items-center gap-2 text-sm text-error"><Icon name="alert" size={15} /> {error}</span>
        <button type="button" class="btn btn-ghost btn-sm" disabled={!reachable} onclick={() => res.refresh()}>Retry</button>
      </div>
    {:else if !hw}
      <p class="text-sm text-base-content/50">
        {reachable ? 'Reading from the device…' : 'Connect the device (Wi-Fi or USB) to read hardware info.'}
      </p>
    {:else}
      {#if diskUsed !== null && diskPct !== null}
        <div class="flex flex-col gap-1.5">
          <div class="flex items-baseline justify-between text-sm">
            <span class="font-medium">Storage</span>
            <span class="text-xs text-base-content/60">
              {formatBytes(diskUsed)} of {formatBytes(hw.diskDataCapacity)} used · {formatBytes(hw.diskDataAvailable)} free
            </span>
          </div>
          <progress
            class={`progress ${diskPct >= 90 ? 'progress-error' : diskPct >= 75 ? 'progress-warning' : 'progress-primary'}`}
            value={diskPct}
            max="100"
          ></progress>
          {#if hw.diskPhotos || hw.diskMedia}
            <div class="flex flex-wrap gap-x-4 gap-y-0.5 text-xs text-base-content/50">
              {#if hw.diskPhotos}<span>Photos {formatBytes(hw.diskPhotos)}</span>{/if}
              {#if hw.diskMedia}<span>Media {formatBytes(hw.diskMedia)}</span>{/if}
            </div>
          {/if}
        </div>
      {/if}

      {#snippet row(r: Row)}
        <div class="flex items-center justify-between gap-2">
          <span class="text-base-content/60">{r.label}</span>
          {#if r.secret && !revealed}
            <span class="font-mono text-xs text-base-content/40">••••••</span>
          {:else}
            <span class={`${r.mono ? 'font-mono text-xs' : ''} ${r.secret ? 'select-all' : ''} ${r.cls ?? ''} truncate`}>{r.value}</span>
          {/if}
        </div>
      {/snippet}

      {#if batteryRows.length > 0 || hw.batterySerial}
        <div class="flex flex-col gap-1.5 border-t border-base-300 pt-3">
          <span class="text-sm font-medium">Battery</span>
          <div class="grid grid-cols-2 gap-x-6 gap-y-1 text-sm sm:grid-cols-3">
            {#each batteryRows as r (r.label)}{@render row(r)}{/each}
          </div>
          {#if hw.batterySerial}
            <div class="text-sm">{@render row({ label: 'Serial', value: hw.batterySerial, mono: true, secret: true })}</div>
          {/if}
        </div>
      {/if}

      {#if idRows.length > 0 || sims.length > 0}
        <div class="flex flex-col gap-1.5 border-t border-base-300 pt-3">
          <div class="flex items-center justify-between">
            <span class="text-sm font-medium">Identity</span>
            <button
              type="button"
              class="btn btn-outline btn-xs"
              aria-pressed={revealed}
              onclick={() => (revealed = !revealed)}
            >
              <Icon name={revealed ? 'eyeOff' : 'eye'} size={14} />
              {revealed ? 'Hide IDs' : 'Reveal IDs'}
            </button>
          </div>
          {#if idRows.length > 0}
            <div class="grid grid-cols-1 gap-x-6 gap-y-1 text-sm sm:grid-cols-2">
              {#each idRows as r (r.label)}{@render row(r)}{/each}
            </div>
          {/if}

          {#if sims.length > 1}
            <div class="grid grid-cols-1 gap-x-6 gap-y-3 sm:grid-cols-2">
              {#each sims as sim, i (i)}
                <div class="flex flex-col gap-1">
                  <span class="text-xs font-medium text-base-content/50">
                    SIM {i + 1}{sim.slot ? ` · ${sim.slot}` : ''}
                  </span>
                  <div class="flex flex-col gap-y-1 border-l border-base-300 pl-3 text-sm">
                    {#if sim.carrier}{@render row({ label: 'Carrier', value: sim.carrier })}{/if}
                    {#if sim.imei}{@render row({ label: 'IMEI', value: sim.imei, mono: true, secret: true })}{/if}
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        </div>
      {/if}
    {/if}
  </div>
</div>
