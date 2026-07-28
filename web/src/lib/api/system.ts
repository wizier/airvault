import { request } from './client';
import type { RunningProgress } from './backups';

export interface Status {
  muxerUp: boolean;
  running: RunningProgress[];
}

export function getStatus(signal?: AbortSignal): Promise<Status> {
  return request<Status>('/status', { signal });
}
