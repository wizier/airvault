import { ApiError, apiUrl, devicePath, request } from './client';
import { endpointFileSource, saveUrl, type FileSource } from './files';
import { gallerySource, type GalleryMonth, type GallerySource } from './gallery';
import type { AcceptedRun } from './runs';

/** Server stage names (see Go service.Stage*); labels live in the UI layer. */
export type RunStage =
  | 'waiting_for_device'
  | 'preparing'
  | 'activating'
  | 'calculating_changes'
  | 'backing_up'
  | 'finalizing'
  | 'restoring'
  | 'cancelling_backup'
  | 'cancelling_restore'
  | 'verifying'
  | 'cancelling_verify';

/** A verify run is an integrity check of the stored backups, not a transfer. */
export type RunKind = 'backup' | 'restore' | 'verify';

export interface RunningProgress {
  runId: string;
  udid: string;
  progress: number;
  stage: RunStage;
  kind: RunKind;
  /** Started by the automatic-backup trigger, not from the UI. */
  auto?: boolean;
  /** A cancel was accepted; the run is winding down. */
  cancelling?: boolean;
  transferred: number;
  speed: number;
}

/** udid/deviceName only in the cross-device restore-sources list. */
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
  /** Why it can be neither restored nor downloaded; absent when it can. */
  damage?: 'files_missing' | 'manifest_unreadable';
  damagedFiles?: number;
  /** When an integrity check last read everything it needs. */
  verifiedAt?: string;
}

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

/** Reads every stored object of the phone's backups and marks the restore
 *  points whose data no longer reads right. */
export function startVerify(udid: string): Promise<AcceptedRun> {
  return request<AcceptedRun>(`${devicePath(udid)}/verify`, { method: 'POST' });
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

/** Smaller than the summed sizes, which count data other restore points share
 *  and keep. */
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

/** A HEAD check first reports a vanished snapshot or an expired session in the
 *  UI; the browser then streams the archive and can resume it. */
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

/** Opens a restore point's files for browsing; an encrypted one needs its
 *  password, and the server then keeps it unlocked while it is browsed. */
export function unlockBackup(snapshotId: string, password: string, signal?: AbortSignal): Promise<void> {
  return request<void>(`/backups/${encodeURIComponent(snapshotId)}/unlock`, {
    method: 'POST',
    body: { password },
    signal,
  });
}

/** Server component names (see Go iosbackup.Component*); labels live in the UI. */
export type BackupComponent = 'photos' | 'messages' | 'contacts' | 'calls';

export interface LabeledValue {
  label?: string;
  value: string;
}

export interface BackupContact {
  name: string;
  organization?: string;
  jobTitle?: string;
  note?: string;
  phones?: LabeledValue[];
  emails?: LabeledValue[];
}

export async function listBackupContacts(snapshotId: string, signal?: AbortSignal): Promise<BackupContact[]> {
  const response = await request<{ contacts: BackupContact[] }>(
    `/backups/${encodeURIComponent(snapshotId)}/contacts`,
    { signal },
  );
  return response.contacts;
}

export interface BackupCall {
  /** A phone number or an email; absent when withheld. */
  address?: string;
  name?: string;
  time: string;
  /** Seconds. */
  duration: number;
  outgoing?: boolean;
  answered?: boolean;
  /** 'phone', 'facetime' or the calling app's bundle ID. */
  service: string;
  video?: boolean;
}

export async function listBackupCalls(snapshotId: string, signal?: AbortSignal): Promise<BackupCall[]> {
  const response = await request<{ calls: BackupCall[] }>(`/backups/${encodeURIComponent(snapshotId)}/calls`, { signal });
  return response.calls;
}

/** A conversation as Messages shows it: one per set of people, over iMessage,
 *  SMS and RCS alike. */
export interface BackupChat {
  /** Its chats in the backup. */
  ids: number[];
  /** The group's name, else who is in it. */
  title: string;
  participants?: { address: string; name?: string }[];
  messages: number;
  last: string;
  /** The last message's text. */
  snippet?: string;
}

export interface ChatAttachment {
  path?: string;
  name: string;
  /** MIME type. */
  type?: string;
  size: number;
  /** Not in the backup: kept only in iCloud. */
  missing?: boolean;
}

export interface ChatMessage {
  id: number;
  text?: string;
  time: string;
  fromMe?: boolean;
  /** The address an incoming one came from. */
  sender?: string;
  /** 'iMessage', 'SMS' or 'RCS': one chat can mix them. */
  service?: string;
  attachments?: ChatAttachment[];
}

export async function listBackupChats(snapshotId: string, signal?: AbortSignal): Promise<BackupChat[]> {
  const response = await request<{ chats: BackupChat[] }>(`/backups/${encodeURIComponent(snapshotId)}/chats`, { signal });
  return response.chats;
}

/** A page of a conversation's messages, the latest first. */
export async function listChatMessages(
  snapshotId: string,
  chat: BackupChat,
  offset: number,
  limit: number,
  signal?: AbortSignal,
): Promise<ChatMessage[]> {
  const q = new URLSearchParams({ offset: String(offset), limit: String(limit) });
  for (const id of chat.ids) q.append('chat', String(id));
  const response = await request<{ messages: ChatMessage[] }>(`/backups/${encodeURIComponent(snapshotId)}/messages?${q}`, {
    signal,
  });
  return response.messages;
}

export function attachmentFiles(snapshotId: string): FileSource {
  return endpointFileSource(`/backups/${encodeURIComponent(snapshotId)}/attachments`);
}

/** The parts of a restore point there are views for, those it holds only. */
export async function listBackupComponents(snapshotId: string, signal?: AbortSignal): Promise<BackupComponent[]> {
  const response = await request<{ components: BackupComponent[] }>(
    `/backups/${encodeURIComponent(snapshotId)}/components`,
    { signal },
  );
  return response.components;
}

// Keys are the server's photo filters (see Go service.photoFilters).
const PHOTO_FILTERS: Record<string, string> = {
  '': 'All',
  favorites: 'Favorites',
  videos: 'Videos',
  live: 'Live Photos',
  hidden: 'Hidden',
  deleted: 'Recently Deleted',
  screenshots: 'Screenshots',
  panoramas: 'Panoramas',
  slomo: 'Slo-mo',
  timelapse: 'Time-lapse',
  screenrecordings: 'Screen Recordings',
};

// The server's filter groups; library filters have none and show as chips.
const FILTER_GROUPS: Record<string, string | undefined> = { media: 'Media Types', album: 'Albums' };

/** A restore point's photo library; an asset's path is its id. */
export function backupGallerySource(snapshotId: string): GallerySource {
  const base = `/backups/${encodeURIComponent(snapshotId)}/photos`;
  return {
    ...gallerySource(base),
    filters: async (signal) => {
      const response = await request<{ filters: { filter: string; group: string; title?: string; count: number }[] }>(
        `${base}/filters`,
        { signal },
      );
      return response.filters.map((f) => ({
        key: f.filter,
        label: f.title ?? PHOTO_FILTERS[f.filter] ?? f.filter,
        count: f.count,
        group: FILTER_GROUPS[f.group],
      }));
    },
    months: async (filter, signal) => {
      const q = filter ? `?${new URLSearchParams({ filter })}` : '';
      return (await request<{ months: GalleryMonth[] }>(`${base}/months${q}`, { signal })).months;
    },
  };
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
