import { request } from './client';

// One camera-roll item. live marks a photo with a paired .MOV (Live Photo).
export interface GalleryAsset {
  path: string; // media-partition path, e.g. "DCIM/100APPLE/IMG_0049.HEIC"
  name: string;
  kind: 'photo' | 'video';
  live?: boolean;
}

interface GalleryPage {
  assets: GalleryAsset[];
  total: number;
  revision: string;
}

function base(udid: string): string {
  return `/devices/${encodeURIComponent(udid)}/media`;
}

interface GalleryPageOptions {
  offset: number;
  limit: number;
  revision?: string;
  signal?: AbortSignal;
}

export async function galleryPage(udid: string, options: GalleryPageOptions): Promise<GalleryPage> {
  const q = new URLSearchParams({ offset: String(options.offset), limit: String(options.limit) });
  if (options.revision) q.set('revision', options.revision);
  return request<GalleryPage>(`${base(udid)}/gallery?${q}`, { signal: options.signal });
}

interface ThumbBatchResponse {
  thumbs: Record<string, string>; // path -> base64 JPEG
}

/** Reads many thumbnails in one request (one AFC session server-side). Returns a
 *  path -> JPEG Blob map; paths with no thumbnail are absent. */
export async function mediaThumbsBatch(
  udid: string,
  paths: string[],
  signal?: AbortSignal,
): Promise<Record<string, Blob>> {
  const response = await request<ThumbBatchResponse>(`${base(udid)}/thumbs`, {
    method: 'POST',
    body: { paths },
    signal,
  });
  const blobs: Record<string, Blob> = {};
  for (const [path, b64] of Object.entries(response.thumbs)) {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    blobs[path] = new Blob([bytes], { type: 'image/jpeg' });
  }
  return blobs;
}

export interface MediaStat {
  size: number;
  modified?: number; // unix seconds; absent when AFC did not report it
}

/** One media file's size and modified time (a single on-demand device stat). */
export async function mediaStat(udid: string, path: string, signal?: AbortSignal): Promise<MediaStat> {
  const q = new URLSearchParams({ path });
  return request<MediaStat>(`${base(udid)}/stat?${q}`, { signal });
}
