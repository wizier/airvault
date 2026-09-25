// SSE client — the primary path for keeping the UI fresh: backend events become
// targeted refreshes (or in-place mutations) of the Resources in stores.svelte.ts.
// start() is ref-counted (one shared, self-reconnecting EventSource).

import type { RunningProgress } from './api/backups';
import type { Connection } from './api/devices';
import type { TrustStatus } from './api/pairing';
import {
  deviceAppsResources,
  devicesStore,
  pairStateStore,
  restoreSourcesStore,
  restorePointResources,
  statusStore,
} from './stores.svelte';

/** Live progress of a device's in-flight backup/restore. statusStore.running is
 *  the single source of truth: seeded by GET /status, mutated by backup.* events. */
export function liveRun(udid: string): RunningProgress | null {
  return statusStore.data?.running.find((r) => r.udid === udid) ?? null;
}

interface PairTrustEvent {
  udid: string;
  status: TrustStatus;
  errorCode?: string;
}

const pairTrustHandlers = new Set<(event: PairTrustEvent) => void>();

/** Receive pair.trust events, the server-side trust flow's progress; returns
 *  the unsubscribe, so `$effect(() => onPairTrust(handler))` works. */
export function onPairTrust(handler: (event: PairTrustEvent) => void): () => void {
  pairTrustHandlers.add(handler);
  return () => pairTrustHandlers.delete(handler);
}

interface InstallProgress {
  phase: 'staging' | 'installing';
  percent: number;
}

/** Live .ipa install phase and percent per install id this browser started. */
export const installProgress = $state<Record<string, InstallProgress>>({});
const activeInstalls = new Set<string>();

/** Accept progress for this id until the returned function clears it. */
export function registerInstall(id: string): () => void {
  activeInstalls.add(id);
  return () => {
    activeInstalls.delete(id);
    delete installProgress[id];
  };
}

// --- SSE payload shapes ----------------------------------------------------

interface DeviceOnlineEvent {
  udid: string;
  connection: Connection;
}
interface DeviceUpdatedEvent {
  udid: string;
  connection?: Connection;
}
interface UdidEvent {
  udid: string;
}
interface RunEvent {
  runId: string;
  udid: string;
  /** Set when the run is a restore onto the device. */
  restore?: boolean;
}
interface RunFailedEvent extends RunEvent {
  errorCode: string;
}

// --- helpers ---------------------------------------------------------------

/** Parse an SSE message's JSON payload, tolerating malformed frames. */
function parse<T>(e: Event): T | null {
  const data = (e as MessageEvent).data;
  if (typeof data !== 'string') return null;
  try {
    return JSON.parse(data) as T;
  } catch {
    return null;
  }
}

/** Register a typed listener for a named SSE event. */
function on<T>(es: EventSource, type: string, handler: (data: T) => void): void {
  es.addEventListener(type, (e: Event) => {
    const data = parse<T>(e);
    if (data !== null) handler(data);
  });
}

/** Coalesce bursty events (e.g. added+updated for the same device) into a
 *  single refetch, so event handling never hammers the backend. */
function debouncedRefresh(store: { refresh(): Promise<void> }, ms = 250): () => void {
  let timer: ReturnType<typeof setTimeout> | null = null;
  return () => {
    if (timer !== null) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = null;
      void store.refresh();
    }, ms);
  };
}

const refreshDevices = debouncedRefresh(devicesStore);
const refreshStatus = debouncedRefresh(statusStore);

// /api/pair/state costs real lockdown traffic to the phone, and the store only
// has subscribers while the pairing modal is open — never refresh it blind.
// (A mount fetches immediately anyway, so nothing is missed while inactive.)
const refreshPairStateDebounced = debouncedRefresh(pairStateStore, 120);
function refreshPairState(): void {
  if (pairStateStore.active) refreshPairStateDebounced();
}

/** Optimistically patch one device's connection so online/offline feels instant;
 *  a debounced refresh confirms the authoritative list shortly after. */
function patchConnection(udid: string, connection: Connection): void {
  devicesStore.mutate((list) =>
    list.map((d) => (d.udid === udid ? { ...d, connection } : d)),
  );
}

/** Pull fresh data after a (re)connect so event-driven stores recover at once
 *  (GET /status also rebuilds live progress — terminal events may have been missed). */
function resyncAll(): void {
  void statusStore.refresh();
  void devicesStore.refresh();
  if (restoreSourcesStore.active) void restoreSourcesStore.refresh();
  restorePointResources.invalidateActive();
  deviceAppsResources.invalidateActive();
}

/** Drop a device's live-run entry from statusStore.running (in place). */
function dropRun(udid: string): void {
  statusStore.mutate((s) => ({ ...s, running: s.running.filter((r) => r.udid !== udid) }));
}

/** Common teardown for terminal backup events. The failed patch is instant feel
 *  only; the server folds the last failed run into the device overview. */
function backupFinished(
  d: { udid: string; restore?: boolean; errorCode?: string },
  state: 'completed' | 'failed' | 'cancelled',
): void {
  dropRun(d.udid);
  if (!d.restore && state === 'completed') {
    restorePointResources.invalidate(d.udid);
    if (restoreSourcesStore.active) void restoreSourcesStore.refresh();
  } else if (state === 'failed') {
    // Optimistic projection of the server's per-kind last-run error; the
    // refetch below confirms it.
    const kind = d.restore ? 'restore' : 'backup';
    devicesStore.mutate((devices) =>
      devices.map((device) =>
        device.udid === d.udid
          ? { ...device, lastRunErrors: { ...device.lastRunErrors, [kind]: d.errorCode ?? `${kind}_failed` } }
          : device,
      ),
    );
  }
  refreshDevices();
  refreshStatus();
}

class EventsClient {
  #es: EventSource | null = null;
  #subscribers = 0;
  #reconnectTimer: ReturnType<typeof setTimeout> | undefined;

  /** Open the shared stream (or join it) and return a teardown that closes it
   *  once the last subscriber leaves: `$effect(() => eventsClient.start())`. */
  start(): () => void {
    this.#subscribers += 1;
    if (this.#es === null) this.#open();

    let stopped = false;
    return () => {
      if (stopped) return;
      stopped = true;
      this.#subscribers -= 1;
      if (this.#subscribers <= 0) {
        this.#subscribers = 0;
        this.#close();
      }
    };
  }

  #open(reconnect = false): void {
    clearTimeout(this.#reconnectTimer);
    const es = new EventSource('/api/events');
    this.#es = es;

    let resync = reconnect;
    es.onopen = () => {
      if (resync) resyncAll();
      resync = true;
    };
    // EventSource retries network drops itself, but a non-200 answer (a 401
    // after a token change, a proxy's 502 during a restart) closes it for good.
    // Reopen it; the status refresh sends a 401 to the login page.
    es.onerror = () => {
      if (es.readyState !== EventSource.CLOSED) return;
      this.#close();
      this.#reconnectTimer = setTimeout(() => this.#open(true), 3_000);
      void statusStore.refresh();
    };

    // --- device lifecycle: list membership / metadata changed ---------------
    on<Record<string, never>>(es, 'stream.reset', () => resyncAll());
    on<UdidEvent>(es, 'device.added', () => refreshDevices());
    on<UdidEvent>(es, 'device.removed', (d) => {
      dropRun(d.udid); // a forgotten device has no live progress
      devicesStore.mutate((devices) => devices.filter((device) => device.udid !== d.udid));
    });
    on<DeviceUpdatedEvent>(es, 'device.updated', (d) => {
      if (d.connection) patchConnection(d.udid, d.connection);
      refreshDevices();
    });

    // --- reachability: patch in-place for instant feel, then confirm --------
    on<DeviceOnlineEvent>(es, 'device.online', (d) => {
      patchConnection(d.udid, d.connection);
      refreshDevices();
    });
    on<UdidEvent>(es, 'device.offline', (d) => {
      patchConnection(d.udid, 'offline');
      refreshDevices();
    });

    // --- backup lifecycle ---------------------------------------------------
    on<RunEvent>(es, 'backup.started', (d) => {
      statusStore.mutate((s) => ({
        ...s,
        running: [
          ...s.running.filter((r) => r.udid !== d.udid),
          // The service's stage events land right after; these match its start states.
          {
            runId: d.runId,
            udid: d.udid,
            progress: 0,
            stage: d.restore ? 'restoring' : 'waiting_for_device',
            restore: d.restore,
            transferred: 0,
            speed: 0,
          },
        ],
      }));
      refreshStatus();
    });
    // Mutate in place only, never refetch — the service already throttles these.
    on<RunningProgress>(es, 'backup.progress', (d) => {
      statusStore.mutate((s) => ({
        ...s,
        running: [...s.running.filter((r) => r.udid !== d.udid), d],
      }));
    });
    on<RunEvent>(es, 'backup.completed', (d) => backupFinished(d, 'completed'));
    on<RunFailedEvent>(es, 'backup.failed', (d) => backupFinished(d, 'failed'));
    on<RunEvent>(es, 'backup.cancelled', (d) => backupFinished(d, 'cancelled'));
    on<UdidEvent>(es, 'backup.catalog', (d) => {
      restorePointResources.invalidate(d.udid);
      if (restoreSourcesStore.active) void restoreSourcesStore.refresh();
      refreshDevices();
    });
    on<UdidEvent>(es, 'app.catalog', (d) => deviceAppsResources.invalidate(d.udid));

    // --- app install progress (only this browser's active install id) --------
    on<{ installId: string } & InstallProgress>(es, 'app.install.progress', (d) => {
      if (activeInstalls.has(d.installId)) installProgress[d.installId] = { phase: d.phase, percent: d.percent };
    });

    // --- pairing wizard -----------------------------------------------------
    on<UdidEvent>(es, 'pair.changed', () => refreshPairState());
    on<PairTrustEvent>(es, 'pair.trust', (d) => {
      for (const handler of pairTrustHandlers) handler(d);
    });

    // --- muxer health -------------------------------------------------------
    // The backend already marked everything offline (device.* events follow);
    // re-read /api/status so the device-bridge pill flips without waiting for a poll.
    // The wizard's state depends on the muxer too — refresh it while mounted.
    on<{ up: boolean }>(es, 'muxer.changed', () => {
      refreshStatus();
      refreshPairState();
    });
  }

  #close(): void {
    clearTimeout(this.#reconnectTimer);
    this.#es?.close();
    this.#es = null;
  }
}

export const eventsClient = new EventsClient();
