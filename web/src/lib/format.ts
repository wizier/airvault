// Small, dependency-free formatting helpers shared across the UI.

/** Middle-truncate a UDID for compact display, keeping head + tail. */
export function shortUdid(udid: string): string {
  if (!udid) return '';
  if (udid.length <= 15) return udid;
  return `${udid.slice(0, 10)}…${udid.slice(-4)}`;
}

/** Compact relative time like "just now", "6m ago", "3h ago", "in 5h". Pass the
 *  live `$now` store to make the label tick; omit it for a static reading. */
export function relativeTime(iso: string, now: number = Date.now()): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '—';

  const deltaSec = Math.round((now - then) / 1000);
  const past = deltaSec >= 0;
  const s = Math.abs(deltaSec);

  let out: string;
  if (s < 45) return past ? 'just now' : 'in a moment';
  else if (s < 5400) out = `${Math.round(s / 60)}m`;
  else if (s < 129600) out = `${Math.round(s / 3600)}h`;
  else if (s < 1728000) out = `${Math.round(s / 86400)}d`;
  else out = `${Math.round(s / 604800)}w`;

  return past ? `${out} ago` : `in ${out}`;
}

/** Absolute, locale-aware date + time. */
export function formatDateTime(iso?: string | null): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' });
}

/** Short date from unix seconds (AFC mtimes). Empty string for 0/unknown. */
export function formatDate(unixSec?: number): string {
  if (!unixSec) return '';
  return new Date(unixSec * 1000).toLocaleDateString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
  });
}

/** Human-readable byte size. Returns "—" for 0/unknown. */
export function formatBytes(n?: number): string {
  if (!n || n <= 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  const digits = v < 10 && i > 0 ? 1 : 0;
  return `${v.toFixed(digits)} ${units[i]}`;
}

/** A file's size and, when known, its modified date: "2.4 MB · Mar 3, 2025". */
export function fileFacts(f: { size?: number; modified?: number }): string {
  return f.modified ? `${formatBytes(f.size)} · ${formatDate(f.modified)}` : formatBytes(f.size);
}

/** Human-readable transfer rate like "12.3 MB/s". Empty string for 0/unknown
 *  so callers can hide the speed line gracefully. */
export function formatSpeed(bytesPerSec?: number): string {
  if (!bytesPerSec || bytesPerSec <= 0) return '';
  return `${formatBytes(bytesPerSec)}/s`;
}

/** Duration between two ISO timestamps, e.g. "4m 12s". */
export function formatDuration(startIso?: string, endIso?: string): string {
  if (!startIso || !endIso) return '—';
  const a = new Date(startIso).getTime();
  const b = new Date(endIso).getTime();
  if (Number.isNaN(a) || Number.isNaN(b) || b < a) return '—';
  let secs = Math.round((b - a) / 1000);
  const h = Math.floor(secs / 3600);
  secs -= h * 3600;
  const m = Math.floor(secs / 60);
  secs -= m * 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${secs}s`;
  return `${secs}s`;
}
