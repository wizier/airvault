import { ApiError, apiUrl, devicePath, request } from './client';
import { saveUrl } from './files';
import type { AcceptedRun } from './runs';

/** Phase of a run, as the server names it (see Go service.Stage*). Labels for
 *  these live in the UI layer; the wire never carries display text. */
export type RunStage =
  | 'waiting_for_device'
  | 'preparing'
  | 'activating'
  | 'calculating_changes'
  | 'backing_up'
  | 'finalizing'
  | 'restoring'
  | 'cancelling_backup'
  | 'cancelling_restore';

export interface RunningProgress {
  runId: string;
  udid: string;
  progress: number;
  stage: RunStage;
  /** The run applies a snapshot onto the device instead of backing it up. */
  restore?: boolean;
  /** Started by the automatic-backup trigger, not from the UI. */
  auto?: boolean;
  /** A cancel was accepted; the run is winding down. */
  cancelling?: boolean;
  transferred: number;
  speed: number;
}

/** One stored backup snapshot; udid/deviceName only in the cross-device
 *  restore-sources list. */
export interface RestorePoint {
  udid?: string;
  snapshotId: string;
  started?: string;
  created: string;
  /** Size of every file, duplicates included — the backup size. */
  sizeBytes: number;
  /** Payload received from the phone while creating this snapshot; unknown after catalog rebuild. */
  transferredBytes: number | null;
  encrypted?: boolean;
  iosVersion?: string;
  deviceName?: string;
}

/** The cross-device list always names each point's source phone. */
export type RestoreSource = RestorePoint & { udid: string };

interface RestorePointsResponse {
  restorePoints: RestorePoint[];
}

interface RestoreRequest {
  snapshotId: string;
  password?: string;
  systemFiles?: boolean;
  reboot?: boolean;
  settingsFromBackup?: boolean;
  removeItemsNotRestored?: boolean;
}

export function startBackup(udid: string): Promise<AcceptedRun> {
  return request<AcceptedRun>(`${devicePath(udid)}/backup`, { method: 'POST' });
}

export async function listRestorePoints(udid: string, signal?: AbortSignal): Promise<RestorePoint[]> {
  const response = await request<RestorePointsResponse>(`${devicePath(udid)}/backups`, { signal });
  return response.restorePoints;
}

export async function deleteDeviceBackups(udid: string): Promise<void> {
  await request<void>(`${devicePath(udid)}/backups?all=true`, {
    method: 'DELETE',
  });
}

export async function deleteSnapshots(udid: string, snapshotIds: string[]): Promise<void> {
  const query = new URLSearchParams(snapshotIds.map((id) => ['id', id]));
  await request<void>(`${devicePath(udid)}/backups?${query}`, {
    method: 'DELETE',
  });
}

/** Bytes freed by deleting these snapshots together — smaller than their summed
 *  sizes, which count data shared with (and kept by) other restore points. */
export async function snapshotsReclaimable(
  udid: string,
  snapshotIds: string[],
  signal?: AbortSignal,
): Promise<number> {
  const query = new URLSearchParams(snapshotIds.map((id) => ['id', id]));
  const response = await request<{ reclaimableBytes: number }>(
    `${devicePath(udid)}/backups/reclaimable?${query}`,
    { signal },
  );
  return response.reclaimableBytes;
}

/** Downloads the snapshot as a Finder-format backup (tar). A HEAD check first
 *  reports a vanished snapshot or an expired session in the UI; the browser then
 *  streams the archive to disk and can resume it if the connection drops. */
export async function downloadBackup(snapshotId: string): Promise<void> {
  const path = `/backups/${encodeURIComponent(snapshotId)}/download`;
  try {
    await request<void>(path, { method: 'HEAD' });
  } catch (err) {
    // A HEAD answer has no body to carry the error code; 404 can only mean this.
    if (err instanceof ApiError && err.status === 404) throw new ApiError(404, 'snapshot_not_found');
    throw err;
  }
  saveUrl(apiUrl(path));
}

export async function listRestoreSources(signal?: AbortSignal): Promise<RestoreSource[]> {
  const response = await request<{ restoreSources: RestoreSource[] }>('/restore-sources', { signal });
  return response.restoreSources;
}

export function startRestore(udid: string, options: RestoreRequest): Promise<AcceptedRun> {
  return request<AcceptedRun>(`${devicePath(udid)}/restore`, {
    method: 'POST',
    body: options,
  });
}
