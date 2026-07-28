import {
  deleteDeviceFile,
  deviceFileDownloadUrl,
  deviceFilePreviewUrl,
  listDeviceFiles,
  type AFCEntry,
} from './device-files';

export type { AFCEntry } from './device-files';

// A file tree the FileBrowser navigates. The optional remove capability lights
// up delete in the UI. list returns one whole directory, folders-first.
export interface FileSource {
  list(path: string, signal?: AbortSignal): Promise<AFCEntry[]>;
  downloadUrl(path: string, downloadId?: string): string;
  previewUrl(path: string): string;
  remove?(path: string): Promise<void>;
}

// An app's Documents container (house_arrest over AFC): download + preview + delete.
export function appFileSource(udid: string, bundleId: string, writable = true): FileSource {
  const base = `/devices/${encodeURIComponent(udid)}/apps/${encodeURIComponent(bundleId)}/files`;
  return {
    list: (path, signal) => listDeviceFiles(base, path, signal),
    downloadUrl: (path, downloadId) => deviceFileDownloadUrl(base, path, downloadId),
    previewUrl: (path) => deviceFilePreviewUrl(base, path),
    ...(writable ? { remove: (path: string) => deleteDeviceFile(base, path) } : {}),
  };
}

// The device media partition (com.apple.afc): download + inline preview, read-only.
export function deviceFileSource(udid: string): FileSource {
  const base = `/devices/${encodeURIComponent(udid)}/media`;
  return {
    list: (path, signal) => listDeviceFiles(base, path, signal),
    downloadUrl: (path, downloadId) => deviceFileDownloadUrl(base, path, downloadId),
    previewUrl: (path) => deviceFilePreviewUrl(base, path),
  };
}
