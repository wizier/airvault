import {
  errorRef,
  errorText,
  type ErrorRef,
  type ErrorTextKey,
} from '../error-text';

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

export function errRef(error: unknown, fallbackCode: ErrorTextKey = 'unknown_error'): ErrorRef {
  if (error instanceof ApiError) return errorRef(error.code, fallbackCode);
  reportUnexpectedError(error);
  return errorRef(fallbackCode, fallbackCode);
}

export function errMsg(error: unknown, fallbackCode: ErrorTextKey = 'unknown_error'): string {
  const ref = errRef(error, fallbackCode);
  return errorText(ref.code, ref.fallbackCode);
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

/** Build the same safe ApiError for JSON requests and attachment downloads. */
export function apiErrorFromBody(
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
  return `${BASE}${path.startsWith('/') ? path : `/${path}`}`;
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

/** POST multipart data with browser-to-server progress. Fetch does not expose
 * upload progress, so this deliberately uses the browser's native XHR channel. */
export function uploadForm(
  path: string,
  body: FormData,
  onProgress: (percent: number) => void,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('POST', apiUrl(path));
    xhr.setRequestHeader('Accept', 'application/json');
    const csrf = cookieValue(CSRF_COOKIE);
    if (csrf) xhr.setRequestHeader('X-CSRF-Token', csrf);

    xhr.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(Math.floor(event.loaded * 100 / event.total));
    };
    xhr.upload.onload = () => onProgress(100);
    xhr.onerror = () => reject(new ApiError(0, 'network_error'));
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve();
        return;
      }
      reject(apiErrorFromBody(xhr.status, parseBody(xhr.responseText).data));
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

  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
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
