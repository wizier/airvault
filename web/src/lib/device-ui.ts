// Shared mappings from backend enums to display tone/label/icon so the device
// tile and the device page stay visually consistent.

import type { RunningProgress } from './api/backups';
import type { BatteryState, Connection, Device } from './api/devices';
import type { IconName } from './components/icons';

export type Tone = 'green' | 'amber' | 'red' | 'slate';

type BackupStatus = 'running' | 'failed' | 'success' | 'never';

/** Backup status as a pure projection of facts the client already holds: a
 *  live run wins, then the last run's failure code, then any restore point. */
export function backupStatus(device: Device, live: RunningProgress | null): BackupStatus {
  if (live !== null) return 'running';
  if (device.lastRunErrors?.backup) return 'failed';
  return device.lastBackup ? 'success' : 'never';
}

export function connectionUi(c: Connection): { tone: Tone; label: string; icon: IconName } {
  switch (c) {
    case 'wifi':
      return { tone: 'green', label: 'Wi-Fi', icon: 'wifi' };
    case 'usb':
      return { tone: 'amber', label: 'USB', icon: 'usb' };
    default:
      return { tone: 'slate', label: 'Offline', icon: 'offline' };
  }
}

/** Battery presentation from a charge reading — icon, tone class, texts. Null
 *  when no reading is available, so callers just hide the block. */
export function batteryUi(
  b: BatteryState | null,
): { icon: IconName; cls: string; label: string; title: string } | null {
  if (!b) return null;

  const { level } = b;
  let icon: IconName;
  let cls: string;

  if (b.charging) {
    icon = 'batteryCharging';
    cls = 'text-success';
  } else if (level >= 70) {
    icon = 'batteryFull';
    cls = 'text-base-content/60';
  } else if (level >= 40) {
    icon = 'batteryMedium';
    cls = 'text-base-content/60';
  } else if (level >= 10) {
    icon = 'batteryLow';
    cls = 'text-warning';
  } else {
    icon = 'battery';
    cls = 'text-error';
  }

  return {
    icon,
    cls,
    label: `${level}%`,
    title: b.charging ? `Battery ${level}% · charging` : `Battery ${level}%`,
  };
}

// Lockdown ProductType → marketing name, iPhone1,1 … iPhone18,5 (iPhone 17e).
const IPHONE_MODELS: Record<string, string> = {
  'iPhone1,1': 'iPhone',
  'iPhone1,2': 'iPhone 3G',
  'iPhone2,1': 'iPhone 3GS',
  'iPhone3,1': 'iPhone 4',
  'iPhone3,2': 'iPhone 4',
  'iPhone3,3': 'iPhone 4',
  'iPhone4,1': 'iPhone 4s',
  'iPhone5,1': 'iPhone 5',
  'iPhone5,2': 'iPhone 5',
  'iPhone5,3': 'iPhone 5c',
  'iPhone5,4': 'iPhone 5c',
  'iPhone6,1': 'iPhone 5s',
  'iPhone6,2': 'iPhone 5s',
  'iPhone7,1': 'iPhone 6 Plus',
  'iPhone7,2': 'iPhone 6',
  'iPhone8,1': 'iPhone 6s',
  'iPhone8,2': 'iPhone 6s Plus',
  'iPhone8,4': 'iPhone SE (2016)',
  'iPhone9,1': 'iPhone 7',
  'iPhone9,2': 'iPhone 7 Plus',
  'iPhone9,3': 'iPhone 7',
  'iPhone9,4': 'iPhone 7 Plus',
  'iPhone10,1': 'iPhone 8',
  'iPhone10,2': 'iPhone 8 Plus',
  'iPhone10,3': 'iPhone X',
  'iPhone10,4': 'iPhone 8',
  'iPhone10,5': 'iPhone 8 Plus',
  'iPhone10,6': 'iPhone X',
  'iPhone11,2': 'iPhone XS',
  'iPhone11,4': 'iPhone XS Max',
  'iPhone11,6': 'iPhone XS Max',
  'iPhone11,8': 'iPhone XR',
  'iPhone12,1': 'iPhone 11',
  'iPhone12,3': 'iPhone 11 Pro',
  'iPhone12,5': 'iPhone 11 Pro Max',
  'iPhone12,8': 'iPhone SE (2020)',
  'iPhone13,1': 'iPhone 12 mini',
  'iPhone13,2': 'iPhone 12',
  'iPhone13,3': 'iPhone 12 Pro',
  'iPhone13,4': 'iPhone 12 Pro Max',
  'iPhone14,2': 'iPhone 13 Pro',
  'iPhone14,3': 'iPhone 13 Pro Max',
  'iPhone14,4': 'iPhone 13 mini',
  'iPhone14,5': 'iPhone 13',
  'iPhone14,6': 'iPhone SE (2022)',
  'iPhone14,7': 'iPhone 14',
  'iPhone14,8': 'iPhone 14 Plus',
  'iPhone15,2': 'iPhone 14 Pro',
  'iPhone15,3': 'iPhone 14 Pro Max',
  'iPhone15,4': 'iPhone 15',
  'iPhone15,5': 'iPhone 15 Plus',
  'iPhone16,1': 'iPhone 15 Pro',
  'iPhone16,2': 'iPhone 15 Pro Max',
  'iPhone17,1': 'iPhone 16 Pro',
  'iPhone17,2': 'iPhone 16 Pro Max',
  'iPhone17,3': 'iPhone 16',
  'iPhone17,4': 'iPhone 16 Plus',
  'iPhone17,5': 'iPhone 16e',
  'iPhone18,1': 'iPhone 17 Pro',
  'iPhone18,2': 'iPhone 17 Pro Max',
  'iPhone18,3': 'iPhone 17',
  'iPhone18,4': 'iPhone Air',
  'iPhone18,5': 'iPhone 17e',
};

/** Human-readable model for a lockdown ProductType ("iPhone16,2" → "iPhone 15
 *  Pro Max"); unknown or non-iPhone identifiers pass through unchanged. */
export function modelDisplayName(productType?: string): string | undefined {
  return productType ? (IPHONE_MODELS[productType] ?? productType) : undefined;
}

/** Display label for a run's stage. The server sends terse phase constants
 *  (see service.Stage*); the passcode hint keys off the raw 'Preparing'. */
export function stageUi(stage: string | undefined, restore?: boolean): string {
  switch (stage) {
    case 'Waiting for device':
      return 'Waiting for the phone to come online…';
    case 'Preparing':
      return 'Preparing on the phone…';
    case 'Activating':
      return 'Activating the phone…';
    case 'Backing up':
      return 'Backing up';
    case 'Finalizing':
      return 'Finalizing the backup locally…';
    case 'Restoring':
      return 'Restoring';
    case 'Cancelling backup':
    case 'Cancelling restore':
      return 'Cancelling…';
    default:
      return restore ? 'Restoring…' : 'Backing up…';
  }
}
