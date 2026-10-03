import type { RunKind } from './backups';
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
  /** Lockdown ActivationState; absent = unknown. */
  activationState?: string;
  lastSeen?: string;
  lastBackup?: string;
  /** Per-kind code of the most recent failed run; the displayed status is
   *  derived client-side. */
  lastRunErrors?: Partial<Record<RunKind, string>>;
  /** Unique object payload referenced by all restore points. */
  diskBytes?: number;
  restorePoints?: number;
  /** Restore points exist on disk but the phone is no longer registered. */
  orphaned?: boolean;
  /** Absent for orphaned sources, which have no settings. */
  autoBackup?: AutoBackupState;
}

export const AUTO_BACKUP_PRESETS = [1, 3, 7] as const;
export type AutoBackupDays = (typeof AUTO_BACKUP_PRESETS)[number];

/** A daily span of the server's time, which may cross midnight. */
export interface AutoBackupWindow {
  /** "HH:MM" in the server's zone. */
  start: string;
  end: string;
}

export interface AutoBackupSettings {
  enabled: boolean;
  everyDays: AutoBackupDays;
  window?: AutoBackupWindow;
}

/** Why the next automatic backup can't start yet. */
export type AutoBackupWait = 'first_backup' | 'schedule' | 'paused' | 'limit';

export interface AutoBackupState extends AutoBackupSettings {
  /** The server's zone (TZ) the window is read in. */
  timeZone: string;
  /** Only while enabled; absent when the next unlock (inside the window)
   *  starts a backup. */
  wait?: AutoBackupWait;
  notBefore?: string;
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

export interface SIM {
  slot?: string;
  carrier?: string;
  imei?: string;
}

export interface HardwareInfo {
  /** Restore and erase are refused while Find My is on. */
  findMyEnabled?: boolean;
  serial?: string;
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
  const query = options.deleteBackups ? '?deleteBackups=true' : '';
  await request<void>(`${devicePath(udid)}/pairing${query}`, { method: 'DELETE' });
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

export async function setAutoBackup(
  udid: string,
  settings: AutoBackupSettings,
  signal?: AbortSignal,
): Promise<void> {
  await request<void>(`${devicePath(udid)}/auto-backup`, { method: 'PUT', body: settings, signal });
}

export async function powerDevice(udid: string, action: PowerAction): Promise<void> {
  await request<void>(`${devicePath(udid)}/power`, {
    method: 'POST',
    body: { action },
  });
}

/** Erases all content and settings, as Finder does; the phone then leaves
 *  AirVault while its backups stay. */
export async function eraseDevice(udid: string): Promise<void> {
  await request<void>(`${devicePath(udid)}/erase`, { method: 'POST' });
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
