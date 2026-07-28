import { apiUrl } from './client';

export type ConsoleLevel = 'notice' | 'info' | 'debug' | 'error' | 'fault';

export interface ConsoleLine {
  ts: string;
  level: ConsoleLevel;
  pid: number;
  image: string;
  message: string;
  subsystem?: string;
  category?: string;
}

export function consoleUrl(udid: string): string {
  return apiUrl(`/devices/${encodeURIComponent(udid)}/console`);
}
