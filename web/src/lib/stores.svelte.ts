// A Resource keeps its last good value across refreshes so the UI never flickers
// to empty; it fetches once per idle→active transition and SSE keeps it fresh.

import { errMsg, isOffline } from './api/client';
import { listApps } from './api/apps';
import {
  deleteDeviceBackups,
  deleteSnapshots,
  listRestorePoints,
  listRestoreSources,
} from './api/backups';
import { eraseDevice, getDeviceBattery, getHardwareInfo, listDevices, unpairDevice, type Device } from './api/devices';
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

  /** The SSE client skips refreshes of inactive resources; a mount refetches
   *  anyway. */
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

  /** Refreshes only while mounted; the next mount fetches anyway. */
  invalidate(): void {
    if (this.active) void this.refresh();
  }

  /** For an already-confirmed projection; the SSE invalidation remains the
   *  canonical refresh. No-op until the first successful load. */
  mutate(updater: (current: T) => T): void {
    if (this.data === null) return;
    this.data = updater(this.data);
  }

  /** Returns a stop function for an $effect. */
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
    this.#entries.get(key)?.invalidate();
  }

  mutate(key: string, updater: (current: T) => T): void {
    this.#entries.get(key)?.mutate(updater);
  }

  invalidateActive(): void {
    for (const resource of this.#entries.values()) resource.invalidate();
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

export function patchDevice(udid: string, patch: (device: Device) => Device): void {
  devicesStore.mutate((devices) => devices.map((d) => (d.udid === udid ? patch(d) : d)));
}

export function dropDevice(udid: string): void {
  devicesStore.mutate((devices) => devices.filter((d) => d.udid !== udid));
}

export async function unpair(udid: string, deleteBackups: boolean): Promise<void> {
  await unpairDevice(udid, { deleteBackups });
  dropDevice(udid);
}

/** The erased phone no longer knows this host, so like an unpaired one it
 *  leaves the list; its backups stay. */
export async function erase(udid: string): Promise<void> {
  await eraseDevice(udid);
  dropDevice(udid);
}

/** An orphaned device exists only through its backups, so it leaves the list. */
export async function deleteAllBackups(device: Device): Promise<void> {
  const udid = device.udid;
  await deleteDeviceBackups(udid);
  restorePointResources.mutate(udid, () => []);
  restoreSourcesStore.mutate((sources) => sources.filter((s) => s.udid !== udid));
  if (device.orphaned) {
    dropDevice(udid);
    return;
  }
  patchDevice(udid, (d) => ({
    ...d,
    diskBytes: undefined,
    restorePoints: undefined,
    lastBackup: undefined,
    lastRunErrors: undefined,
  }));
}

/** Disk space frees in the background; a later backup.catalog event refreshes sizes. */
export async function deleteRestorePoints(udid: string, snapshotIds: string[]): Promise<void> {
  await deleteSnapshots(udid, snapshotIds);
  const deleted = new Set(snapshotIds);
  restorePointResources.mutate(udid, (points) => points.filter((p) => !deleted.has(p.snapshotId)));
  restoreSourcesStore.mutate((sources) => sources.filter((s) => !deleted.has(s.snapshotId)));
}
