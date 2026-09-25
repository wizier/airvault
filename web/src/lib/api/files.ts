import { apiUrl, request } from './client';

// One entry in an AFC-backed directory listing. size/modified are absent for
// directories (AFC only reports them for files).
export interface AFCEntry {
  name: string;
  kind: 'file' | 'directory';
  size?: number;
  modified?: number;
}

export interface FileStat {
  size: number;
  modified?: number; // unix seconds; absent when AFC did not report it
}

// A file tree the FileBrowser navigates. The optional remove capability lights
// up delete in the UI. list returns one whole directory, folders-first.
export interface FileSource {
  list(path: string, signal?: AbortSignal): Promise<AFCEntry[]>;
  /** One file's size and modified time (a single device stat). */
  stat(path: string, signal?: AbortSignal): Promise<FileStat>;
  /** An attachment URL the browser downloads natively. */
  downloadUrl(path: string): string;
  /** An <img> src: native image bytes or a server-side HEIC transcode. */
  previewUrl(path: string): string;
  remove?(path: string): Promise<void>;
}

/** Base of the device media partition endpoints (files and gallery). */
export function mediaBase(udid: string): string {
  return `/devices/${encodeURIComponent(udid)}/media`;
}

// The AFC file endpoints share one shape across the media partition and app
// Documents; only the base path differs.
function afcFileSource(base: string, writable: boolean): FileSource {
  const at = (endpoint: string, path: string) => `${base}${endpoint}?${new URLSearchParams({ path })}`;
  return {
    list: async (path, signal) => (await request<{ entries: AFCEntry[] }>(at('', path), { signal })).entries,
    stat: (path, signal) => request<FileStat>(at('/stat', path), { signal }),
    downloadUrl: (path) => apiUrl(at('/download', path)),
    previewUrl: (path) => apiUrl(at('/preview', path)),
    ...(writable ? { remove: (path: string) => request<void>(at('', path), { method: 'DELETE' }) } : {}),
  };
}

// An app's Documents container (house_arrest over AFC): download + preview + delete.
export function appFileSource(udid: string, bundleId: string, writable = true): FileSource {
  return afcFileSource(`/devices/${encodeURIComponent(udid)}/apps/${encodeURIComponent(bundleId)}/files`, writable);
}

// The device media partition (com.apple.afc): download + inline preview, read-only.
export function deviceFileSource(udid: string): FileSource {
  return afcFileSource(mediaBase(udid), false);
}

/** Stats the file first, so an offline, locked or busy phone reports in the UI
 *  rather than as a failed browser download, then hands the download to the
 *  browser, which streams it to disk. */
export async function downloadFile(source: FileSource, path: string, name: string, signal: AbortSignal): Promise<void> {
  await source.stat(path, signal);
  if (signal.aborted) return;
  const link = document.createElement('a');
  link.href = source.downloadUrl(path);
  link.download = name;
  // Attached for the click: older Firefox ignores clicks on detached anchors.
  document.body.append(link);
  link.click();
  link.remove();
}
