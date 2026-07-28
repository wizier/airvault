import { request } from './client';

export interface AcceptedRun {
  runId: string;
}

export async function cancelRun(runId: string): Promise<void> {
  await request<void>(`/runs/${encodeURIComponent(runId)}/cancel`, { method: 'POST' });
}
