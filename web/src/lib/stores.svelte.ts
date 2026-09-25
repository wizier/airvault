// Shared live-state Resources built on Svelte 5 runes: ref-counted start(), last
// good value kept across refreshes so the UI never flickers to empty. One fetch
// per idle→active transition; SSE pushes keep them fresh while subscribed.

import { errMsg, isOffline } from './api/client';
import { listApps } from './api/apps';
import {
  deleteDeviceBackups,
  deleteSnapshots,
  listRestorePoints,
  listRestoreSources,
} from './api/backups';
import { getDeviceBattery, getHardwareInfo, listDevices, unpairDevice, type Device } from './api/devices';
import { getPairState } from './api/pairing';
import { getStatus } from './api/system';
import type { ErrorTextKey } from './error-text';

class Resource<T> {
  data = $state.raw<T | null>(null);
  loading = $state(false);
  error = $state<unknown | null>(null);

  #fetcher: (signal?: AbortSignal) => Promise<T>;
  #subscribers = 0;
  #inflight: AbortController | null = null;
  #work: Promise<void> | null = null;
  #requested = 0;
  #settled = 0;

  constructor(fetcher: (signal?: AbortSignal) => Promise<T>) {
    this.#fetcher = fetcher;
  }

  /** Whether we have ever successfully loaded data. */
  get ready(): boolean {
    return this.data !== null;
  }

  get offline(): boolean {
    return isOffline(this.error);
  }

  /** Message for a failed first load; null while offline, which the App banner covers. */
  loadError(fallback: ErrorTextKey): string | null {
    return !this.ready && this.error !== null && !this.offline ? errMsg(this.error, fallback) : null;
  }

  /** Whether any component is currently subscribed. The SSE client skips
   *  refreshes of inactive resources — a mount refetches anyway. */
  get active(): boolean {
    return this.#subscribers > 0;
  }

  /** Concurrent HTTP/SSE refreshes share one request; if state changes during
   * that request, exactly one follow-up fetch closes the race instead of
   * cancel/restart thrashing. */
  async refresh(): Promise<void> {
    this.#requested += 1;
    return this.#drain();
  }

  #drain(): Promise<void> {
    if (this.#work !== null) return this.#work;
    this.#work = this.#run().finally(() => {
      this.#work = null;
      if (this.#settled < this.#requested && this.#subscribers > 0) void this.#drain();
    });
    return this.#work;
  }

  async #run(): Promise<void> {
    while (this.#settled < this.#requested) {
      const target = this.#requested;
      const ctrl = new AbortController();
      this.#inflight = ctrl;
      this.loading = true;
      this.error = null;
      try {
        const next = await this.#fetcher(ctrl.signal);
        if (!ctrl.signal.aborted) this.data = next;
      } catch (err) {
        if (!ctrl.signal.aborted) this.error = err;
        // Deliberately keep this.data (last good value) on failure.
      } finally {
        this.#settled = target;
        if (this.#inflight === ctrl) this.#inflight = null;
      }
      if (ctrl.signal.aborted && this.#subscribers === 0) break;
    }
    this.loading = false;
  }

  /** Apply an already-confirmed HTTP or pushed-event projection immediately;
   *  the SSE invalidation remains the cross-client canonical refresh. No-op
   *  until the first successful load. */
  mutate(updater: (current: T) => T): void {
    if (this.data === null) return;
    this.data = updater(this.data);
  }

  /** Begin observing this resource (shared across components); returns a stop
   *  function for an $effect. Fetches on each idle→active transition — SSE
   *  keeps it fresh while active and resyncs on reconnect. */
  start(): () => void {
    const wasIdle = this.#subscribers === 0;
    this.#subscribers += 1;
    if (wasIdle) void this.refresh();

    let stopped = false;
    return () => {
      if (stopped) return;
      stopped = true;
      this.#subscribers = Math.max(0, this.#subscribers - 1);
      if (this.#subscribers === 0) this.#inflight?.abort();
    };
  }
}

class KeyedResources<T> {
  #entries = new Map<string, Resource<T>>();
  #fetcher: (key: string, signal?: AbortSignal) => Promise<T>;

  constructor(fetcher: (key: string, signal?: AbortSignal) => Promise<T>) {
    this.#fetcher = fetcher;
  }

  for(key: string): Resource<T> {
    let resource = this.#entries.get(key);
    if (!resource) {
      resource = new Resource((signal) => this.#fetcher(key, signal));
      this.#entries.set(key, resource);
    }
    return resource;
  }

  invalidate(key: string): void {
    const resource = this.#entries.get(key);
    if (resource?.active) void resource.refresh();
  }

  mutate(key: string, updater: (current: T) => T): void {
    this.#entries.get(key)?.mutate(updater);
  }

  invalidateActive(): void {
    for (const resource of this.#entries.values()) {
      if (resource.active) void resource.refresh();
    }
  }
}

export const statusStore = new Resource(getStatus);
export const devicesStore = new Resource(listDevices);
export const restoreSourcesStore = new Resource(listRestoreSources);
export const restorePointResources = new KeyedResources(listRestorePoints);
export const deviceAppsResources = new KeyedResources(listApps);
// A live hardware snapshot per device; read on demand (Refresh re-reads). Keyed
// by udid so switching devices swaps to a fresh Resource, never stale identity.
export const hardwareResources = new KeyedResources(getHardwareInfo);
// Charge status is client-owned live telemetry, kept per-udid outside the
// wholesale-replaced device list so a list refresh never clobbers a fresh poll.
export const batteryResources = new KeyedResources(getDeviceBattery);
// Pairing-wizard device state: pair.changed / pair.trust / muxer.changed SSE
// events drive refreshes — each pair-state read costs real lockdown traffic.
export const pairStateStore = new Resource(getPairState);

// Mutations below apply their confirmed result to every affected list at once;
// the SSE events that follow confirm it idempotently.

export async function unpair(udid: string, deleteBackups: boolean): Promise<void> {
  await unpairDevice(udid, { deleteBackups });
  devicesStore.mutate((devices) => devices.filter((d) => d.udid !== udid));
}

/** An orphaned device exists only through its backups, so it leaves the list. */
export async function deleteAllBackups(device: Device): Promise<void> {
  const udid = device.udid;
  await deleteDeviceBackups(udid);
  restorePointResources.mutate(udid, () => []);
  restoreSourcesStore.mutate((sources) => sources.filter((s) => s.udid !== udid));
  devicesStore.mutate((devices) =>
    device.orphaned
      ? devices.filter((d) => d.udid !== udid)
      : devices.map((d) =>
          d.udid === udid
            ? { ...d, diskBytes: undefined, restorePoints: undefined, lastBackup: undefined, lastRunErrors: undefined }
            : d,
        ),
  );
}

/** Disk space frees in the background; a later backup.catalog event refreshes sizes. */
export async function deleteRestorePoints(udid: string, snapshotIds: string[]): Promise<void> {
  await deleteSnapshots(udid, snapshotIds);
  const deleted = new Set(snapshotIds);
  restorePointResources.mutate(udid, (points) => points.filter((p) => !deleted.has(p.snapshotId)));
  restoreSourcesStore.mutate((sources) => sources.filter((s) => !deleted.has(s.snapshotId)));
}
