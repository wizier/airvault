import { apiUrl, request, uploadForm } from './client';

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

/** User apps installed on the device (the endpoint never lists system apps). */
export async function listApps(udid: string, signal?: AbortSignal): Promise<DeviceApp[]> {
  const response = await request<ListAppsResponse>(`/devices/${encodeURIComponent(udid)}/apps`, { signal });
  return response.apps;
}

export async function installApp(
  udid: string,
  file: File,
  installId: string,
  onProgress: (percent: number) => void,
): Promise<void> {
  const form = new FormData();
  form.append('ipa', file);
  const query = new URLSearchParams({ installId });
  await uploadForm(`/devices/${encodeURIComponent(udid)}/apps/install?${query}`, form, onProgress);
}

export async function uninstallApp(udid: string, bundleId: string): Promise<void> {
  await request<void>(`/devices/${encodeURIComponent(udid)}/apps/${encodeURIComponent(bundleId)}`, {
    method: 'DELETE',
  });
}

export function appIconUrl(udid: string, bundleId: string): string {
  return apiUrl(`/devices/${encodeURIComponent(udid)}/apps/${encodeURIComponent(bundleId)}/icon`);
}
