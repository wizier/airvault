import { apiErrorFromBody, errRef, type ApiError } from './api/client';
import { clientId } from './client-id';
import type { ErrorRef } from './error-text';
import { downloadProgress, registerDownload } from './events.svelte';

function saveBlob(blob: Blob, filename: string): void {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Let the browser consume the synthetic click before releasing the blob URL.
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

async function responseError(res: Response): Promise<ApiError> {
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    /* non-JSON body */
  }
  return apiErrorFromBody(res.status, body);
}

// A reusable set of in-flight downloads keyed by a caller-chosen key. Each fetches
// an attachment URL to a blob and saves it, reporting per-item SSE progress (via
// events.downloadProgress) and supporting cancel. Shared by the file browser and
// the gallery so both get identical progress + cancel behaviour.
export class Downloads {
  #active = $state<Record<string, { id: string; ctrl: AbortController }>>({});
  #onError: (error: ErrorRef) => void;

  constructor(onError: (error: ErrorRef) => void = () => {}) {
    this.#onError = onError;
  }

  active(key: string): boolean {
    return !!this.#active[key];
  }

  // Download percent 0-100, or null before the first SSE frame (→ show a spinner).
  percent(key: string): number | null {
    const d = this.#active[key];
    if (!d) return null;
    return downloadProgress[d.id] ?? null;
  }

  cancel(key: string): void {
    const download = this.#active[key];
    if (!download) return;
    download.ctrl.abort();
  }

  cancelAll(): void {
    for (const key of Object.keys(this.#active)) this.cancel(key);
  }

  // urlFor receives a freshly minted download id to embed so the server can report
  // pull progress over SSE for this exact download.
  async start(key: string, urlFor: (downloadId: string) => string, filename: string): Promise<void> {
    if (this.#active[key]) return;
    const id = clientId('download');
    const ctrl = new AbortController();
    const unregister = registerDownload(id);
    this.#active[key] = { id, ctrl };
    try {
      const res = await fetch(urlFor(id), { signal: ctrl.signal });
      if (!res.ok) throw await responseError(res);
      saveBlob(await res.blob(), filename);
    } catch (err) {
      if ((err as Error)?.name !== 'AbortError') this.#onError(errRef(err, 'download_failed'));
    } finally {
      unregister();
      delete this.#active[key];
    }
  }
}
