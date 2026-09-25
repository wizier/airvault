import { apiUrl, devicePath, request } from './client';

export type Connection = 'wifi' | 'usb' | 'offline';

export interface Device {
  udid: string;
  name: string;
  productType?: string;
  iosVersion?: string;
  connection: Connection;
  paired: boolean;
  encrypted: boolean;
  lockScreen?: boolean;
  /** Lockdown ActivationState ("Activated", "Unactivated", ...); absent = unknown. */
  activationState?: string;
  lastSeen?: string;
  lastBackup?: string;
  /** Per-kind stable error code of the device's most recent failed run
   *  (server runtime state); the displayed status is derived client-side. */
  lastRunErrors?: { backup?: string; restore?: string };
  /** Unique object payload referenced by all restore points. */
  diskBytes?: number;
  /** How many validated backup snapshots are kept. */
  restorePoints?: number;
  /** Restore points exist on disk but the phone is no longer registered. */
  orphaned?: boolean;
}

export interface BatteryState {
  charging: boolean;
  level: number;
}

interface ListDevicesResponse {
  devices: Device[];
}

interface UnpairOptions {
  deleteBackups?: boolean;
}

export type PowerAction = 'restart' | 'shutdown' | 'sleep';

/** One cellular slot: friendly carrier ("MTS (RU)") and that slot's IMEI. */
export interface SIM {
  slot?: string;
  carrier?: string;
  imei?: string;
}

export interface HardwareInfo {
  /** Find My iPhone state — the phone refuses any restore while it is on. */
  findMyEnabled?: boolean;
  serial?: string;
  /** Lockdown identifier, e.g. "iPhone16,2". */
  productType?: string;
  modelNumber?: string;
  hardwareModel?: string;
  region?: string;
  wifiMac?: string;
  bluetoothMac?: string;
  phoneNumber?: string;
  sims?: SIM[];
  buildVersion?: string;
  timeZone?: string;
  diskDataCapacity?: number;
  diskDataAvailable?: number;
  diskPhotos?: number;
  diskMedia?: number;
  batteryHealthPct?: number;
  batteryCycles?: number;
  batteryDesignCapacity?: number;
  batteryMaxCapacity?: number;
  batteryVoltageMv?: number;
  batteryAmperageMa?: number;
  /** Centi-degrees Celsius, e.g. 3012 = 30.12 °C. */
  batteryTemperature?: number;
  batterySerial?: string;
}

export async function listDevices(signal?: AbortSignal): Promise<Device[]> {
  const response = await request<ListDevicesResponse>('/devices', { signal });
  return response.devices;
}

export async function unpairDevice(udid: string, options: UnpairOptions = {}): Promise<void> {
  const query = new URLSearchParams();
  if (options.deleteBackups) query.set('deleteBackups', 'true');
  const encodedQuery = query.toString();
  const suffix = encodedQuery ? `?${encodedQuery}` : '';
  await request<void>(`${devicePath(udid)}/pairing${suffix}`, { method: 'DELETE' });
}

export async function changeBackupPassword(
  udid: string,
  oldPassword: string,
  newPassword: string,
  signal?: AbortSignal,
): Promise<void> {
  await request<void>(`${devicePath(udid)}/backup-password`, {
    method: 'POST',
    body: { old: oldPassword, new: newPassword },
    signal,
  });
}

export async function powerDevice(udid: string, action: PowerAction): Promise<void> {
  await request<void>(`${devicePath(udid)}/power`, {
    method: 'POST',
    body: { action },
  });
}

export function getHardwareInfo(udid: string, signal?: AbortSignal): Promise<HardwareInfo> {
  return request<HardwareInfo>(`${devicePath(udid)}/hardware`, { signal });
}

export function getDeviceBattery(udid: string, signal?: AbortSignal): Promise<BatteryState> {
  return request<BatteryState>(`${devicePath(udid)}/battery`, { signal });
}

export function wallpaperUrl(udid: string, screen: 'home' | 'lock'): string {
  return apiUrl(`${devicePath(udid)}/wallpaper?screen=${screen}`);
}
