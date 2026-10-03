import { request } from './client';
import { endpointFileSource, type FileSource } from './files';
import { gallerySource, type GalleryMonth, type GallerySource } from './gallery';

/** What a restore point holds, browsed under /backups/<id>. */
const backupPath = (snapshotId: string) => `/backups/${encodeURIComponent(snapshotId)}`;

async function list<T>(snapshotId: string, what: string, signal?: AbortSignal): Promise<T[]> {
  return (await request<Record<string, T[]>>(`${backupPath(snapshotId)}/${what}`, { signal }))[what];
}

/** Opens a restore point's files for browsing; an encrypted one needs its
 *  password, and the server then keeps it unlocked while it is browsed. */
export function unlockBackup(snapshotId: string, password: string, signal?: AbortSignal): Promise<void> {
  return request<void>(`${backupPath(snapshotId)}/unlock`, { method: 'POST', body: { password }, signal });
}

/** Server component names (see Go iosbackup.Component*); labels live in the UI. */
export type BackupComponent = 'photos' | 'messages' | 'whatsapp' | 'notes' | 'contacts' | 'calls';

/** The messaging apps a backup holds chats of. */
export type ChatApp = 'messages' | 'whatsapp';

/** The parts of a restore point there are views for, those it holds only. */
export const listBackupComponents = (snapshotId: string, signal?: AbortSignal) =>
  list<BackupComponent>(snapshotId, 'components', signal);

/** The files a component's items carry, by path. */
export function backupFiles(snapshotId: string, component: BackupComponent): FileSource {
  return endpointFileSource(`${backupPath(snapshotId)}/${component}/files`);
}

/** A file a message or a note carries: one without a path is a link, a table
 *  or kept only in iCloud; a scanned document has pages. */
export interface BackupAttachment {
  name: string;
  path?: string;
  size?: number;
  missing?: boolean;
  pages?: BackupAttachment[];
}

export interface BackupContact {
  name: string;
  organization?: string;
  jobTitle?: string;
  note?: string;
  phones?: { label?: string; value: string }[];
  emails?: { label?: string; value: string }[];
}

export const listBackupContacts = (snapshotId: string, signal?: AbortSignal) =>
  list<BackupContact>(snapshotId, 'contacts', signal);

export interface BackupNote {
  id: number;
  title: string;
  folder?: string;
  modified?: string;
  /** Holds U+FFFC where each of the attachments sits, in their order. */
  text?: string;
  attachments?: BackupAttachment[];
  /** Password-protected: its text stays encrypted. */
  locked?: boolean;
}

export const listBackupNotes = (snapshotId: string, signal?: AbortSignal) => list<BackupNote>(snapshotId, 'notes', signal);

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

export const listBackupCalls = (snapshotId: string, signal?: AbortSignal) => list<BackupCall>(snapshotId, 'calls', signal);

export interface Participant {
  address: string;
  name?: string;
  /** A picture among the files. */
  avatar?: string;
}

/** A conversation as the app shows it: Messages joins one person's iMessage,
 *  SMS and RCS chats, WhatsApp their chats under a number and a LID. */
export interface BackupChat {
  /** Its chats in the backup. */
  ids: number[];
  /** The group's name, else who is in it. */
  title: string;
  participants?: Participant[];
  /** A picture among the files. */
  avatar?: string;
  /** How many shown, when the app counts them. */
  messages?: number;
  last: string;
  /** The last message's text. */
  snippet?: string;
}

export interface ChatMessage {
  id: number;
  text?: string;
  time: string;
  fromMe?: boolean;
  /** The address an incoming one came from. */
  sender?: string;
  /** 'iMessage', 'SMS' or 'RCS', which one chat can mix, or 'WhatsApp'. */
  service?: string;
  attachments?: BackupAttachment[];
  /** What else it is (see Go iosbackup.Message): 'call', 'event', 'location', 'contact', 'poll', 'deleted',
   *  'waiting', 'viewOncePhoto', 'viewOnceVideo' or 'viewOnceVoice'. */
  kind?: string;
  call?: Pick<BackupCall, 'duration' | 'outgoing' | 'answered' | 'video'>;
  event?: ChatEvent;
  location?: { latitude: number; longitude: number; name?: string };
}

/** A line a chat shows about itself; codes as in Go iosbackup.ChatEvent. No actor is the backup's owner. */
export interface ChatEvent {
  code: string;
  actor?: Participant;
  targets?: Participant[];
  text?: string;
}

export const listBackupChats = (snapshotId: string, app: ChatApp, signal?: AbortSignal) =>
  list<BackupChat>(snapshotId, `${app}/chats`, signal);

/** A page of a conversation's messages, the latest first. */
export async function listBackupMessages(
  snapshotId: string,
  app: ChatApp,
  chat: BackupChat,
  offset: number,
  limit: number,
  signal?: AbortSignal,
): Promise<ChatMessage[]> {
  const q = new URLSearchParams({ offset: String(offset), limit: String(limit) });
  for (const id of chat.ids) q.append('chat', String(id));
  const response = await request<{ messages: ChatMessage[] }>(`${backupPath(snapshotId)}/${app}/chats/messages?${q}`, {
    signal,
  });
  return response.messages;
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
  const base = `${backupPath(snapshotId)}/photos`;
  return {
    ...gallerySource(base),
    files: backupFiles(snapshotId, 'photos'),
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
