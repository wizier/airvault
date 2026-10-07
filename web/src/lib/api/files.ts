import { apiUrl, devicePath, request } from './client';

// size/modified are absent for directories: AFC only reports them for files.
export interface AFCEntry {
  name: string;
  kind: 'file' | 'directory';
  size?: number;
  modified?: number;
  /** Backup files only: listed, but the backup does not hold the content. */
  missing?: boolean;
}

export interface FileStat {
  size: number;
  modified?: number; // unix seconds; absent when AFC did not report it
}

export interface FileSource {
  list(path: string, signal?: AbortSignal): Promise<AFCEntry[]>;
  stat(path: string, signal?: AbortSignal): Promise<FileStat>;
  downloadUrl(path: string): string;
  previewUrl(path: string): string;
  remove?(path: string): Promise<void>;
}

/** Files served by ?path=… at base/stat, base/download and base/preview; the
 *  listing, where there is one, at base itself. */
export function endpointFileSource(base: string, writable = false): FileSource {
  const at = (endpoint: string, path: string) => `${base}${endpoint}?${new URLSearchParams({ path })}`;
  return {
    list: async (path, signal) => (await request<{ entries: AFCEntry[] }>(at('', path), { signal })).entries,
    stat: (path, signal) => request<FileStat>(at('/stat', path), { signal }),
    downloadUrl: (path) => apiUrl(at('/download', path)),
    previewUrl: (path) => apiUrl(at('/preview', path)),
    ...(writable ? { remove: (path: string) => request<void>(at('', path), { method: 'DELETE' }) } : {}),
  };
}

export function appFileSource(udid: string, bundleId: string, writable: boolean): FileSource {
  return endpointFileSource(`${devicePath(udid)}/apps/${encodeURIComponent(bundleId)}/files`, writable);
}

export function deviceFileSource(udid: string): FileSource {
  return endpointFileSource(`${devicePath(udid)}/media`);
}

/** Stats the file first, so an offline, locked or busy phone reports in the UI
 *  rather than as a failed browser download. */
export async function downloadFile(source: FileSource, path: string, name: string, signal: AbortSignal): Promise<void> {
  await source.stat(path, signal);
  if (signal.aborted) return;
  saveUrl(source.downloadUrl(path), name);
}

/** An empty name keeps the server's Content-Disposition file name. */
export function saveUrl(url: string, name = ''): void {
  const link = document.createElement('a');
  link.href = url;
  link.download = name;
  // Attached for the click: older Firefox ignores clicks on detached anchors.
  document.body.append(link);
  link.click();
  link.remove();
}
