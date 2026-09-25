import { errorText, type ErrorTextKey } from '../error-text';

const BASE = '/api';
const CSRF_COOKIE = '_csrf';

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  signal?: AbortSignal;
  /** Don't bounce to the login page on 401 — used by the login/logout calls. */
  skipAuthRedirect?: boolean;
}

/** A 401 means the session is gone; send the user to the hash-routed login. */
function redirectToLogin(): void {
  const current = window.location.hash.replace(/^#/, '');
  if (current.startsWith('/login')) return;
  window.location.hash = '#/login';
}

/** Error returned for an HTTP failure or when the backend cannot be reached. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code = 'unknown_error', options?: ErrorOptions) {
    super(code, options);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }

  get offline(): boolean {
    return this.status === 0;
  }
}

export function isOffline(error: unknown): boolean {
  return error instanceof ApiError && error.offline;
}

export function isAbortError(error: unknown): boolean {
  return typeof error === 'object' && error !== null && 'name' in error && error.name === 'AbortError';
}

const reportedUnexpectedErrors = new WeakSet<object>();

function reportUnexpectedError(error: unknown): void {
  if (typeof error === 'object' && error !== null) {
    if (reportedUnexpectedErrors.has(error)) return;
    reportedUnexpectedErrors.add(error);
  }
  // Native/library wording belongs in diagnostics, never in user-facing copy.
  console.error('Unexpected client error', error);
}

/** The stable code of an API error; anything else is reported and mapped to the fallback. */
export function errorCode(error: unknown, fallbackCode: ErrorTextKey): string {
  if (error instanceof ApiError) return error.code;
  reportUnexpectedError(error);
  return fallbackCode;
}

export function errMsg(error: unknown, fallbackCode: ErrorTextKey = 'unknown_error'): string {
  return errorText(errorCode(error, fallbackCode), fallbackCode);
}

/** Fallback for responses without a backend error envelope (proxies, gateways);
 *  backend JSON bodies always carry the precise error.code. */
function statusErrorCode(status: number): string {
  switch (status) {
    case 502:
      return 'upstream_error';
    case 503:
      return 'service_unavailable';
    default:
      return status >= 500 ? 'internal_error' : `http_${status}`;
  }
}

/** Build the same safe ApiError for fetch and XHR responses. */
function apiErrorFromBody(
  status: number,
  body: unknown,
  skipAuthRedirect = false,
): ApiError {
  if (status === 401 && !skipAuthRedirect) redirectToLogin();
  let code = statusErrorCode(status);
  if (body && typeof body === 'object' && 'error' in body) {
    const payload = (body as { error: unknown }).error;
    if (payload && typeof payload === 'object' && 'code' in payload) {
      const candidate = (payload as { code: unknown }).code;
      if (typeof candidate === 'string') code = candidate;
    }
  }
  return new ApiError(status, code);
}

export function apiUrl(path: string): string {
  return `${BASE}${path}`;
}

function cookieValue(name: string): string | undefined {
  const prefix = `${encodeURIComponent(name)}=`;
  const entry = document.cookie
    .split(';')
    .map((part) => part.trim())
    .find((part) => part.startsWith(prefix));
  return entry ? decodeURIComponent(entry.slice(prefix.length)) : undefined;
}

/** Parse a response body: JSON when possible; plain-text and empty bodies are
 * kept raw as valid diagnostics. */
function parseBody(text: string): { data: unknown; json: boolean } {
  try {
    return { data: JSON.parse(text), json: true };
  } catch {
    return { data: text, json: false };
  }
}

/** POST multipart data with browser-to-server progress, then read the server's
 * NDJSON progress stream: each line goes to onServerProgress until a
 * {"done":true} line resolves or an {"error":…} line rejects. A non-200 answer
 * is an ordinary JSON error. Fetch does not expose upload progress, so this
 * deliberately uses the browser's native XHR channel. */
export function uploadForm<P>(
  path: string,
  body: FormData,
  onUploadProgress: (percent: number) => void,
  onServerProgress: (progress: P) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', apiUrl(path));
    xhr.setRequestHeader('Accept', 'application/x-ndjson, application/json');
    const csrf = cookieValue(CSRF_COOKIE);
    if (csrf) xhr.setRequestHeader('X-CSRF-Token', csrf);

    // responseText grows as the stream arrives; `consumed` marks the end of
    // the last complete line already handled.
    let consumed = 0;
    function readLines(): void {
      if (xhr.status !== 200) return;
      const end = xhr.responseText.lastIndexOf('\n') + 1;
      const lines = xhr.responseText.slice(consumed, end).split('\n');
      consumed = end;
      for (const line of lines) {
        if (!line) continue;
        const { data, json } = parseBody(line);
        if (!json || !data || typeof data !== 'object') reject(new ApiError(xhr.status, 'invalid_response'));
        else if ('error' in data) reject(apiErrorFromBody(xhr.status, data));
        else if ('done' in data) resolve();
        else onServerProgress(data as P);
      }
    }

    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onUploadProgress(Math.floor(event.loaded * 100 / event.total));
    };
    xhr.upload.onload = () => onUploadProgress(100);
    xhr.onprogress = readLines;
    xhr.onerror = () => reject(new ApiError(0, 'network_error'));
    xhr.onload = () => {
      if (xhr.status !== 200) {
        reject(apiErrorFromBody(xhr.status, parseBody(xhr.responseText).data));
        return;
      }
      readLines();
      // No-op once settled; a stream without its done/error line is malformed.
      reject(new ApiError(xhr.status, 'invalid_response'));
    };
    xhr.send(body);
  });
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET';
  const init: RequestInit = {
    method,
    headers: { Accept: 'application/json' },
    signal: options.signal,
  };

  if (method !== 'GET') {
    const csrf = cookieValue(CSRF_COOKIE);
    if (csrf) init.headers = { ...init.headers, 'X-CSRF-Token': csrf };
  }

  if (options.body !== undefined) {
    init.body = JSON.stringify(options.body);
    init.headers = { ...init.headers, 'Content-Type': 'application/json' };
  }

  let response: Response;
  try {
    response = await fetch(apiUrl(path), init);
  } catch (error) {
    // Cancellation is control flow, not a backend outage.
    if (options.signal?.aborted || isAbortError(error)) throw error;
    throw new ApiError(0, 'network_error', { cause: error });
  }

  const body = parseBody(await response.text());

  if (!response.ok) {
    throw apiErrorFromBody(response.status, body.data, options.skipAuthRedirect);
  }

  if (response.status === 204) return undefined as T;
  if (!body.json) {
    throw new ApiError(response.status, 'invalid_response');
  }

  return body.data as T;
}
