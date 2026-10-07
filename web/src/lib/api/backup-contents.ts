import { apiUrl, request } from './client';
import { endpointFileSource, type FileSource } from './files';
import { gallerySource, type GalleryMonth, type GallerySource } from './gallery';

/** What a restore point holds, browsed under /backups/<id>. */
export const backupPath = (snapshotId: string) => `/backups/${encodeURIComponent(snapshotId)}`;

/** A list comes as {"<the path's last segment>": […]}. */
async function list<T>(snapshotId: string, what: string, signal?: AbortSignal, query?: URLSearchParams): Promise<T[]> {
  const key = what.slice(what.lastIndexOf('/') + 1);
  const q = query?.size ? `?${query}` : '';
  return (await request<Record<string, T[]>>(`${backupPath(snapshotId)}/${what}${q}`, { signal }))[key];
}

/** Opens a restore point's files for browsing; an encrypted one needs its
 *  password, and the server then keeps it unlocked while it is browsed. */
export function unlockBackup(snapshotId: string, password: string, signal?: AbortSignal): Promise<void> {
  return request<void>(`${backupPath(snapshotId)}/unlock`, { method: 'POST', body: { password }, signal });
}

/** Server component names (see Go iosbackup.Component*); labels live in the UI. */
export type BackupComponent = 'photos' | 'messages' | 'whatsapp' | 'notes' | 'contacts' | 'calls' | 'passwords' | 'files';

/** The messaging apps a backup holds chats of. */
export type ChatApp = 'messages' | 'whatsapp';

/** The parts of a restore point there are views for, those it holds only. */
export const listBackupComponents = (snapshotId: string, signal?: AbortSignal) =>
  list<BackupComponent>(snapshotId, 'components', signal);

/** A component's files by path, and its folders by the same paths. */
export function backupFiles(snapshotId: string, component: BackupComponent): FileSource {
  return endpointFileSource(`${backupPath(snapshotId)}/${component}/files`);
}

/** Where a person's or a chat's picture is. */
export interface Picture {
  /** A file among the app's. */
  avatar?: string;
  /** The contact whose photo shows them, when it has one. */
  contactId?: number;
}

export function pictureUrl(snapshotId: string, component: BackupComponent, p?: Picture): string | undefined {
  if (p?.avatar) return backupFiles(snapshotId, component).previewUrl(p.avatar);
  return p?.contactId ? apiUrl(`${backupPath(snapshotId)}/contacts/${p.contactId}/photo`) : undefined;
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

export interface BackupContact extends Picture {
  id: number;
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

export interface BackupCall extends Picture {
  /** A phone number or an email; absent when withheld. */
  address?: string;
  name?: string;
  /** None for a call a chat records. */
  time?: string;
  /** Seconds. */
  duration: number;
  outgoing?: boolean;
  answered?: boolean;
  /** 'phone', 'facetime' or the calling app's bundle ID. */
  service: string;
  /** The calling app's name, when the backup holds the app. */
  app?: string;
  video?: boolean;
}

export const listBackupCalls = (snapshotId: string, signal?: AbortSignal) => list<BackupCall>(snapshotId, 'calls', signal);

/** A saved password from the keychain: a Wi-Fi network, or an app's or a site's. */
export interface BackupSecret {
  id: number;
  kind: 'wifi' | 'app' | 'web';
  /** The network name, the service or the site. */
  title: string;
  /** The user name, when the item names one. */
  account?: string;
  password: string;
  /** A site's URL scheme: 'https', 'smb', … */
  protocol?: string;
  /** A site's port, when the item names one. */
  port?: number;
  /** A site's sign-in: 'form', 'basic', 'digest', … */
  auth?: string;
  /** What saved it: Safari, an app's name, else its bundle ID or keychain group. */
  app?: string;
  /** The app's, when the backup holds its icon. */
  bundleId?: string;
}

/** The icon of an app the backup holds. */
export const backupAppIconUrl = (snapshotId: string, bundleId: string) =>
  apiUrl(`${backupPath(snapshotId)}/apps/${encodeURIComponent(bundleId)}/icon`);

/** The saved passwords the backup holds, Wi-Fi networks first. */
export const listBackupPasswords = (snapshotId: string, signal?: AbortSignal) =>
  list<BackupSecret>(snapshotId, 'passwords', signal);

export interface Participant extends Picture {
  address: string;
  name?: string;
}

/** A conversation as the app shows it: Messages joins one person's iMessage,
 *  SMS and RCS chats, WhatsApp their chats under a number and a LID. */
export interface BackupChat extends Picture {
  /** Its chats in the backup. */
  ids: number[];
  /** The group's name, else who is in it. */
  title: string;
  participants?: Participant[];
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

/** The query naming a conversation's chats, with params. */
function chatQuery(chat: BackupChat, params: Record<string, string>): URLSearchParams {
  const q = new URLSearchParams(params);
  for (const id of chat.ids) q.append('chat', String(id));
  return q;
}

/** A page of a conversation's messages, the latest first. */
export function listBackupMessages(
  snapshotId: string,
  app: ChatApp,
  chat: BackupChat,
  offset: number,
  limit: number,
  signal?: AbortSignal,
): Promise<ChatMessage[]> {
  const q = chatQuery(chat, { offset: String(offset), limit: String(limit) });
  return list<ChatMessage>(snapshotId, `${app}/chats/messages`, signal, q);
}

/** A message a search found in a conversation, where it stands in its pages. */
export interface ChatMatch {
  id: number;
  offset: number;
}

/** The messages of a conversation that show query, whatever its case, the latest first. */
export const searchBackupChat = (
  snapshotId: string,
  app: ChatApp,
  chat: BackupChat,
  query: string,
  signal?: AbortSignal,
) => list<ChatMatch>(snapshotId, `${app}/chats/matches`, signal, chatQuery(chat, { q: query }));

/** A message a search found, in the chat with the ID `chat`. */
export interface FoundMessage extends Pick<ChatMessage, 'id' | 'text' | 'time' | 'fromMe'> {
  chat: number;
}

/** An app's messages that show query, whatever its case: the latest 200. */
export const searchBackupMessages = (snapshotId: string, app: ChatApp, query: string, signal?: AbortSignal) =>
  list<FoundMessage>(snapshotId, `${app}/matches`, signal, new URLSearchParams({ q: query }));

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
  return {
    ...gallerySource(`${backupPath(snapshotId)}/photos`),
    files: backupFiles(snapshotId, 'photos'),
    filters: async (signal) => {
      const filters = await list<{ filter: string; group: string; title?: string; count: number }>(
        snapshotId,
        'photos/filters',
        signal,
      );
      return filters.map((f) => ({
        key: f.filter,
        label: f.title ?? PHOTO_FILTERS[f.filter] ?? f.filter,
        count: f.count,
        group: FILTER_GROUPS[f.group],
      }));
    },
    months: (filter, signal) =>
      list<GalleryMonth>(snapshotId, 'photos/months', signal, new URLSearchParams(filter ? { filter } : {})),
  };
}
