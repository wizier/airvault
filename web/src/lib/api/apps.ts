import { request, uploadForm } from './client';

export interface DeviceApp {
  bundleId: string;
  name: string;
  version?: string;
  /** The app exposes its Documents over house_arrest (gates the Files action). */
  fileSharing?: boolean;
}

interface ListAppsResponse {
  apps: DeviceApp[];
}

/** The phone's side of an install; percent is local to the phase. */
export interface InstallProgress {
  phase: 'staging' | 'installing';
  percent: number;
}

/** User apps installed on the device (the endpoint never lists system apps). */
export async function listApps(udid: string, signal?: AbortSignal): Promise<DeviceApp[]> {
  const response = await request<ListAppsResponse>(`/devices/${encodeURIComponent(udid)}/apps`, { signal });
  return response.apps;
}

/** Uploads and installs an .ipa; resolves once the phone reports it installed. */
export async function installApp(
  udid: string,
  file: File,
  onUploadProgress: (percent: number) => void,
  onDeviceProgress: (progress: InstallProgress) => void,
): Promise<void> {
  const form = new FormData();
  form.append('ipa', file);
  await uploadForm(`/devices/${encodeURIComponent(udid)}/apps/install`, form, onUploadProgress, onDeviceProgress);
}

export async function uninstallApp(udid: string, bundleId: string): Promise<void> {
  await request<void>(`/devices/${encodeURIComponent(udid)}/apps/${encodeURIComponent(bundleId)}`, {
    method: 'DELETE',
  });
}

interface AppIconsResponse {
  icons: Record<string, string>; // bundle id -> base64 PNG
}

/** Reads many app icons in one request (one springboard connection server-side).
 *  Returns a bundle id -> PNG data URL map; apps without an icon are absent. */
export async function appIcons(
  udid: string,
  bundleIds: string[],
  signal?: AbortSignal,
): Promise<Record<string, string>> {
  const response = await request<AppIconsResponse>(`/devices/${encodeURIComponent(udid)}/apps/icons`, {
    method: 'POST',
    body: { bundleIds },
    signal,
  });
  const urls: Record<string, string> = {};
  for (const [bundleId, b64] of Object.entries(response.icons)) urls[bundleId] = `data:image/png;base64,${b64}`;
  Object.assign(cachedAppIcons(udid), urls);
  return urls;
}

// Icons live for the page's lifetime, so reopening the Apps modal is instant.
// Only real icons are kept: a missing one is asked for again next time.
const iconCache = new Map<string, Record<string, string>>();

/** Icons already loaded for a device: bundle id -> PNG data URL. */
export function cachedAppIcons(udid: string): Record<string, string> {
  let icons = iconCache.get(udid);
  if (!icons) iconCache.set(udid, (icons = {}));
  return icons;
}
