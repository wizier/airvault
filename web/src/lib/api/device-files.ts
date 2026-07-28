import { apiUrl, request } from './client';

type EntryKind = 'file' | 'directory';

// One entry in an AFC-backed directory listing. size/modified are absent for
// directories (AFC only reports them for files).
export interface AFCEntry {
  name: string;
  kind: EntryKind;
  size?: number;
  modified?: number;
}

// One AFC directory, returned whole and ordered folders-first then by name.
interface DeviceFilesResponse {
  entries: AFCEntry[];
}

// The AFC file endpoints share one shape across the media partition and app
// Documents; only the base path differs. These builders take that base so both
// FileSource factories reuse one implementation.

export async function listDeviceFiles(base: string, path: string, signal?: AbortSignal): Promise<AFCEntry[]> {
  const query = new URLSearchParams({ path });
  const response = await request<DeviceFilesResponse>(`${base}?${query}`, { signal });
  return response.entries;
}

/** Download URL for one file. downloadId lets the server report pull progress
 *  over SSE; the client fetches this (not a navigation) and saves the blob. */
export function deviceFileDownloadUrl(base: string, path: string, downloadId?: string): string {
  const query = new URLSearchParams({ path });
  if (downloadId) query.set('downloadId', downloadId);
  return apiUrl(`${base}/download?${query}`);
}

/** Inline-preview URL for one image (used as an <img> src): native image bytes
 *  or a server-side HEIC transcode. */
export function deviceFilePreviewUrl(base: string, path: string): string {
  const query = new URLSearchParams({ path });
  return apiUrl(`${base}/preview?${query}`);
}

export async function deleteDeviceFile(base: string, path: string): Promise<void> {
  const query = new URLSearchParams({ path });
  await request<void>(`${base}?${query}`, { method: 'DELETE' });
}
