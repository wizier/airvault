import { request } from './client';
import type { AcceptedRun } from './runs';

interface PairUsbDevice {
  udid: string;
  name: string;
}

export interface PairState {
  muxerReady: boolean;
  usbDevices: PairUsbDevice[];
}

export type TrustStatus = 'paired' | 'trust_pending' | 'locked' | 'denied' | 'error';

export function getPairState(signal?: AbortSignal): Promise<PairState> {
  return request<PairState>('/pair/state', { signal });
}

export function startTrust(udid: string): Promise<AcceptedRun> {
  return request<AcceptedRun>('/pair/trust', { method: 'POST', body: { udid } });
}
