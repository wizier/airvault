// Shared mappings from backend enums to display tone/label/icon so the device
// tile and the device page stay visually consistent.

import type { RunningProgress, RunStage } from './api/backups';
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

/** How a model's body is drawn in the preview — a tablet is not a big phone. */
export interface DeviceScreen {
  form: 'phone' | 'tablet';
  /** Screen aspect in portrait (width / height); the frame's proportions. */
  ratio: number;
}

const PHONE: DeviceScreen = { form: 'phone', ratio: 462 / 978 };
// 4:3 — every iPad through the 10.2", plus the whole 12.9"/13" line.
const TABLET_43: DeviceScreen = { form: 'tablet', ratio: 3 / 4 };
// 10.9"/11" edge-to-edge; the Pro's 1668×2388 is within a percent of this.
const TABLET_11: DeviceScreen = { form: 'tablet', ratio: 1640 / 2360 };
const TABLET_MINI: DeviceScreen = { form: 'tablet', ratio: 1488 / 2266 };

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

// iPads, split by the body the preview draws. Wi-Fi, cellular and China
// variants of one model share a name because they share that body.
const IPAD_43_MODELS: Record<string, string> = {
  'iPad1,1': 'iPad',
  'iPad2,1': 'iPad 2',
  'iPad2,2': 'iPad 2',
  'iPad2,3': 'iPad 2',
  'iPad2,4': 'iPad 2',
  'iPad2,5': 'iPad mini',
  'iPad2,6': 'iPad mini',
  'iPad2,7': 'iPad mini',
  'iPad3,1': 'iPad (3rd gen)',
  'iPad3,2': 'iPad (3rd gen)',
  'iPad3,3': 'iPad (3rd gen)',
  'iPad3,4': 'iPad (4th gen)',
  'iPad3,5': 'iPad (4th gen)',
  'iPad3,6': 'iPad (4th gen)',
  'iPad4,1': 'iPad Air',
  'iPad4,2': 'iPad Air',
  'iPad4,3': 'iPad Air',
  'iPad4,4': 'iPad mini 2',
  'iPad4,5': 'iPad mini 2',
  'iPad4,6': 'iPad mini 2',
  'iPad4,7': 'iPad mini 3',
  'iPad4,8': 'iPad mini 3',
  'iPad4,9': 'iPad mini 3',
  'iPad5,1': 'iPad mini 4',
  'iPad5,2': 'iPad mini 4',
  'iPad5,3': 'iPad Air 2',
  'iPad5,4': 'iPad Air 2',
  'iPad6,3': 'iPad Pro 9.7″',
  'iPad6,4': 'iPad Pro 9.7″',
  'iPad6,7': 'iPad Pro 12.9″',
  'iPad6,8': 'iPad Pro 12.9″',
  'iPad6,11': 'iPad (5th gen)',
  'iPad6,12': 'iPad (5th gen)',
  'iPad7,1': 'iPad Pro 12.9″ (2nd gen)',
  'iPad7,2': 'iPad Pro 12.9″ (2nd gen)',
  'iPad7,3': 'iPad Pro 10.5″',
  'iPad7,4': 'iPad Pro 10.5″',
  'iPad7,5': 'iPad (6th gen)',
  'iPad7,6': 'iPad (6th gen)',
  'iPad7,11': 'iPad (7th gen)',
  'iPad7,12': 'iPad (7th gen)',
  'iPad8,5': 'iPad Pro 12.9″ (3rd gen)',
  'iPad8,6': 'iPad Pro 12.9″ (3rd gen)',
  'iPad8,7': 'iPad Pro 12.9″ (3rd gen)',
  'iPad8,8': 'iPad Pro 12.9″ (3rd gen)',
  'iPad8,11': 'iPad Pro 12.9″ (4th gen)',
  'iPad8,12': 'iPad Pro 12.9″ (4th gen)',
  'iPad11,1': 'iPad mini (5th gen)',
  'iPad11,2': 'iPad mini (5th gen)',
  'iPad11,3': 'iPad Air (3rd gen)',
  'iPad11,4': 'iPad Air (3rd gen)',
  'iPad11,6': 'iPad (8th gen)',
  'iPad11,7': 'iPad (8th gen)',
  'iPad12,1': 'iPad (9th gen)',
  'iPad12,2': 'iPad (9th gen)',
  'iPad13,8': 'iPad Pro 12.9″ (5th gen)',
  'iPad13,9': 'iPad Pro 12.9″ (5th gen)',
  'iPad13,10': 'iPad Pro 12.9″ (5th gen)',
  'iPad13,11': 'iPad Pro 12.9″ (5th gen)',
  'iPad14,5': 'iPad Pro 12.9″ (6th gen)',
  'iPad14,6': 'iPad Pro 12.9″ (6th gen)',
  'iPad14,10': 'iPad Air 13″ (M2)',
  'iPad14,11': 'iPad Air 13″ (M2)',
  'iPad15,5': 'iPad Air 13″ (M3)',
  'iPad15,6': 'iPad Air 13″ (M3)',
  'iPad16,5': 'iPad Pro 13″ (M4)',
  'iPad16,6': 'iPad Pro 13″ (M4)',
};

const IPAD_11_MODELS: Record<string, string> = {
  'iPad8,1': 'iPad Pro 11″',
  'iPad8,2': 'iPad Pro 11″',
  'iPad8,3': 'iPad Pro 11″',
  'iPad8,4': 'iPad Pro 11″',
  'iPad8,9': 'iPad Pro 11″ (2nd gen)',
  'iPad8,10': 'iPad Pro 11″ (2nd gen)',
  'iPad13,1': 'iPad Air (4th gen)',
  'iPad13,2': 'iPad Air (4th gen)',
  'iPad13,4': 'iPad Pro 11″ (3rd gen)',
  'iPad13,5': 'iPad Pro 11″ (3rd gen)',
  'iPad13,6': 'iPad Pro 11″ (3rd gen)',
  'iPad13,7': 'iPad Pro 11″ (3rd gen)',
  'iPad13,16': 'iPad Air (5th gen)',
  'iPad13,17': 'iPad Air (5th gen)',
  'iPad13,18': 'iPad (10th gen)',
  'iPad13,19': 'iPad (10th gen)',
  'iPad14,3': 'iPad Pro 11″ (4th gen)',
  'iPad14,4': 'iPad Pro 11″ (4th gen)',
  'iPad14,8': 'iPad Air 11″ (M2)',
  'iPad14,9': 'iPad Air 11″ (M2)',
  'iPad15,3': 'iPad Air 11″ (M3)',
  'iPad15,4': 'iPad Air 11″ (M3)',
  'iPad15,7': 'iPad (A16)',
  'iPad15,8': 'iPad (A16)',
  'iPad16,3': 'iPad Pro 11″ (M4)',
  'iPad16,4': 'iPad Pro 11″ (M4)',
};

const IPAD_MINI_MODELS: Record<string, string> = {
  'iPad14,1': 'iPad mini (6th gen)',
  'iPad14,2': 'iPad mini (6th gen)',
  'iPad16,1': 'iPad mini (A17 Pro)',
  'iPad16,2': 'iPad mini (A17 Pro)',
};

/** One lookup over every table above: name for the label, screen for the frame. */
const MODELS = new Map<string, { name: string; screen: DeviceScreen }>(
  (
    [
      [PHONE, IPHONE_MODELS],
      [TABLET_43, IPAD_43_MODELS],
      [TABLET_11, IPAD_11_MODELS],
      [TABLET_MINI, IPAD_MINI_MODELS],
    ] as const
  ).flatMap(([screen, models]) =>
    Object.entries(models).map(([productType, name]) => [productType, { name, screen }] as const),
  ),
);

/** Human-readable model for a lockdown ProductType ("iPhone16,2" → "iPhone 15
 *  Pro Max"); unknown or non-Apple identifiers pass through unchanged. */
export function modelDisplayName(productType?: string): string | undefined {
  return productType ? (MODELS.get(productType)?.name ?? productType) : undefined;
}

/** Body the preview draws; a model newer than this build falls back by family. */
export function deviceScreen(productType?: string): DeviceScreen {
  const known = productType ? MODELS.get(productType) : undefined;
  if (known) return known.screen;
  return isTablet(productType) ? TABLET_11 : PHONE;
}

export function deviceIcon(productType?: string): IconName {
  return isTablet(productType) ? 'tablet' : 'phone';
}

/** iPadOS only split from iOS in 13 — older iPads still report plain iOS. */
export function osName(productType?: string, version?: string): string {
  return isTablet(productType) && Number.parseInt(version ?? '', 10) >= 13 ? 'iPadOS' : 'iOS';
}

function isTablet(productType?: string): boolean {
  return productType?.startsWith('iPad') ?? false;
}

/** An ellipsis marks a phase the progress bar cannot measure. Record, not a
 *  switch: a stage added to RunStage without a label stops compiling. */
const STAGE_LABELS: Record<RunStage, string> = {
  waiting_for_device: 'Waiting for the phone to come online…',
  preparing: 'Preparing on the phone…',
  activating: 'Activating the phone…',
  calculating_changes: 'Calculating changes on the phone…',
  backing_up: 'Backing up',
  finalizing: 'Finalizing the backup locally…',
  restoring: 'Restoring',
  cancelling_backup: 'Cancelling…',
  cancelling_restore: 'Cancelling…',
};

/** A server ahead of this client can name a stage it has no label for. */
export function stageUi(stage: RunStage | undefined, restore?: boolean): string {
  return (stage && STAGE_LABELS[stage]) || (restore ? 'Restoring…' : 'Backing up…');
}
