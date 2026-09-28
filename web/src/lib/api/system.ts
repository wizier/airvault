import { request } from './client';
import type { RunningProgress } from './backups';

/** What netmuxd last proved: whether it answers and the devices it sees. */
export interface MuxerStatus {
  up: boolean;
  usb: number;
  wifi: number;
  error?: string;
}

export interface Status {
  muxer: MuxerStatus;
  running: RunningProgress[];
}

export function getStatus(signal?: AbortSignal): Promise<Status> {
  return request<Status>('/status', { signal });
}
