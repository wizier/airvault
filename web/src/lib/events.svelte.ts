import type { RunKind, RunningProgress, RunStage } from './api/backups';
import type { Connection } from './api/devices';
import type { TrustStatus } from './api/pairing';
import type { MuxerStatus } from './api/system';
import {
  deviceAppsResources,
  devicesStore,
  dropDevice,
  pairStateStore,
  patchDevice,
  restoreSourcesStore,
  restorePointResources,
  statusStore,
} from './stores.svelte';

/** statusStore.running is the single source of truth: seeded by GET /status,
 *  mutated by backup.* events. */
export function liveRun(udid: string): RunningProgress | null {
  return statusStore.data?.running.find((r) => r.udid === udid) ?? null;
}

interface PairTrustEvent {
  udid: string;
  status: TrustStatus;
  errorCode?: string;
}

const pairTrustHandlers = new Set<(event: PairTrustEvent) => void>();

/** Returns the unsubscribe, so `$effect(() => onPairTrust(handler))` works. */
export function onPairTrust(handler: (event: PairTrustEvent) => void): () => void {
  pairTrustHandlers.add(handler);
  return () => pairTrustHandlers.delete(handler);
}

// Event payloads; internal/service/events.go defines them.
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
  kind: RunKind;
  /** Set on a terminal event of a run the automatic-backup trigger started. */
  auto?: boolean;
}
interface RunFailedEvent extends RunEvent {
  errorCode: string;
}

function parse<T>(e: Event): T | null {
  const data = (e as MessageEvent).data;
  if (typeof data !== 'string') return null;
  try {
    return JSON.parse(data) as T;
  } catch {
    return null;
  }
}

function on<T>(es: EventSource, type: string, handler: (data: T) => void): void {
  es.addEventListener(type, (e: Event) => {
    const data = parse<T>(e);
    if (data !== null) handler(data);
  });
}

/** Coalesces bursty events (e.g. added+updated for one device) into one refetch. */
function debounced(refresh: () => void, ms = 250): () => void {
  let timer: ReturnType<typeof setTimeout> | undefined;
  return () => {
    clearTimeout(timer);
    timer = setTimeout(refresh, ms);
  };
}

const refreshDevices = debounced(() => void devicesStore.refresh());
const refreshStatus = debounced(() => void statusStore.refresh());

// /api/pair/state costs real lockdown traffic, so it refreshes only while the
// pairing modal is mounted; a mount fetches anyway, so nothing is missed.
const refreshPairState = debounced(() => pairStateStore.invalidate(), 120);

/** Optimistic; a debounced refresh confirms the authoritative list shortly after. */
function patchConnection(udid: string, connection: Connection): void {
  patchDevice(udid, (d) => ({ ...d, connection }));
}

/** GET /status also rebuilds live progress: terminal events may have been missed. */
function resyncAll(): void {
  void statusStore.refresh();
  void devicesStore.refresh();
  restoreSourcesStore.invalidate();
  restorePointResources.invalidateActive();
  deviceAppsResources.invalidateActive();
}

// The service's stage events land right after; these match its start states.
const START_STAGE: Record<RunKind, RunStage> = {
  backup: 'waiting_for_device',
  restore: 'restoring',
  verify: 'verifying',
};

function dropRun(udid: string): void {
  statusStore.mutate((s) => ({ ...s, running: s.running.filter((r) => r.udid !== udid) }));
}

/** The failed patch is instant feel only; the server folds the last failed run
 *  into the device overview. */
function backupFinished(
  d: RunEvent & { errorCode?: string },
  state: 'completed' | 'failed' | 'cancelled',
): void {
  dropRun(d.udid);
  if (d.kind !== 'restore' && state === 'completed') {
    restorePointResources.invalidate(d.udid);
    restoreSourcesStore.invalidate();
  } else if (state === 'failed' && !(d.auto && d.errorCode === 'backup_not_confirmed')) {
    // Optimistic projection of the server's per-kind last-run error; the
    // refetch below confirms it. An unanswered automatic prompt is not one:
    // it only pauses the trigger.
    patchDevice(d.udid, (device) => ({
      ...device,
      lastRunErrors: { ...device.lastRunErrors, [d.kind]: d.errorCode ?? `${d.kind}_failed` },
    }));
  }
  refreshDevices();
  refreshStatus();
}

class EventsClient {
  #es: EventSource | null = null;
  #subscribers = 0;
  #reconnectTimer: ReturnType<typeof setTimeout> | undefined;

  /** Ref-counted: `$effect(() => eventsClient.start())`; the last subscriber to
   *  leave closes the stream. */
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

    on<Record<string, never>>(es, 'stream.reset', () => resyncAll());
    on<UdidEvent>(es, 'device.added', () => refreshDevices());
    on<UdidEvent>(es, 'device.removed', (d) => {
      dropRun(d.udid); // a forgotten device has no live progress
      dropDevice(d.udid);
    });
    on<DeviceUpdatedEvent>(es, 'device.updated', (d) => {
      if (d.connection) patchConnection(d.udid, d.connection);
      refreshDevices();
    });

    on<DeviceOnlineEvent>(es, 'device.online', (d) => {
      patchConnection(d.udid, d.connection);
      refreshDevices();
    });
    on<UdidEvent>(es, 'device.offline', (d) => {
      patchConnection(d.udid, 'offline');
      refreshDevices();
    });

    on<RunEvent>(es, 'backup.started', (d) => {
      statusStore.mutate((s) => ({
        ...s,
        running: [
          ...s.running.filter((r) => r.udid !== d.udid),
          {
            runId: d.runId,
            udid: d.udid,
            progress: 0,
            stage: START_STAGE[d.kind],
            kind: d.kind,
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
      restoreSourcesStore.invalidate();
      refreshDevices();
    });
    on<UdidEvent>(es, 'backup.unlocked', (d) => restorePointResources.invalidate(d.udid));
    on<UdidEvent>(es, 'app.catalog', (d) => deviceAppsResources.invalidate(d.udid));

    on<UdidEvent>(es, 'pair.changed', () => refreshPairState());
    on<PairTrustEvent>(es, 'pair.trust', (d) => {
      for (const handler of pairTrustHandlers) handler(d);
    });

    // The event carries the whole muxer status (device.* events follow a loss);
    // the wizard's state depends on it too.
    on<MuxerStatus>(es, 'muxer.changed', (muxer) => {
      statusStore.mutate((s) => ({ ...s, muxer }));
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
