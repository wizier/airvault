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
  damagedPoints?: number;
  /** Restore points exist on disk but the phone is no longer registered. */
  orphaned?: boolean;
  /** autoBackup, cleanup and autoVerify are absent for orphaned sources, which
   *  have no settings. */
  autoBackup?: ScheduleState<AutoBackupDays>;
  cleanup?: CleanupSettings;
  autoVerify?: ScheduleState<AutoVerifyDays>;
}

export const AUTO_BACKUP_PRESETS = [1, 3, 7] as const;
export type AutoBackupDays = (typeof AUTO_BACKUP_PRESETS)[number];

export const AUTO_VERIFY_PRESETS = [7, 14, 30] as const;
export type AutoVerifyDays = (typeof AUTO_VERIFY_PRESETS)[number];

/** A daily span of the server's time, which may cross midnight. */
export interface AutoWindow {
  /** "HH:MM" in the server's zone. */
  start: string;
  end: string;
}

export interface ScheduleSettings<Days extends number> {
  enabled: boolean;
  everyDays: Days;
  window?: AutoWindow;
}

/** Why the next run of a scheduled task can't start yet. */
export type ScheduleWait = 'first_backup' | 'schedule' | 'paused' | 'limit';

export interface ScheduleState<Days extends number> extends ScheduleSettings<Days> {
  /** The server's zone (TZ) the window is read in. */
  timeZone: string;
  /** Only while enabled; absent when only the window and the task's own trigger
   *  hold it. */
  wait?: ScheduleWait;
  notBefore?: string;
}

export const CLEANUP_DAY_PRESETS = [7, 14, 30, 60] as const;
export type CleanupDays = (typeof CLEANUP_DAY_PRESETS)[number];

/** How often a backup older than the latest ones stays. */
export type ThinPeriod = 'week' | 'month' | 'none';

export interface CleanupSettings {
  enabled: boolean;
  keepDays: CleanupDays;
  thin: ThinPeriod;
}

export interface AutomaticTasks {
  autoBackup: ScheduleSettings<AutoBackupDays>;
  cleanup: CleanupSettings;
  autoVerify: ScheduleSettings<AutoVerifyDays>;
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

/** All of them or none are saved. Each backup of the phone applies the cleanup settings. */
export async function setAutomaticTasks(udid: string, tasks: AutomaticTasks, signal?: AbortSignal): Promise<void> {
  await request<void>(`${devicePath(udid)}/automatic-tasks`, { method: 'PUT', body: tasks, signal });
}

/** The restore points cleanup with these settings would remove now. */
export async function planCleanup(
  udid: string,
  { keepDays, thin }: Omit<CleanupSettings, 'enabled'>,
  signal?: AbortSignal,
): Promise<string[]> {
  const query = new URLSearchParams({ keepDays: String(keepDays), thin });
  const response = await request<{ remove: string[] }>(`${devicePath(udid)}/cleanup/plan?${query}`, { signal });
  return response.remove;
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
