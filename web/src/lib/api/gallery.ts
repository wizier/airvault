import { devicePath, request } from './client';

export interface GalleryAsset {
  path: string;
  name: string;
  kind: 'photo' | 'video';
  live?: boolean;
}

interface GalleryPage {
  assets: GalleryAsset[];
  total: number;
  revision: string;
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
  return request<GalleryPage>(`${devicePath(udid)}/media/gallery?${q}`, { signal: options.signal });
}

interface ThumbBatchResponse {
  thumbs: Record<string, string>; // path -> base64 JPEG
}

/** Paths with no thumbnail are absent from the result. */
export async function mediaThumbsBatch(
  udid: string,
  paths: string[],
  signal?: AbortSignal,
): Promise<Record<string, string>> {
  const response = await request<ThumbBatchResponse>(`${devicePath(udid)}/media/thumbs`, {
    method: 'POST',
    body: { paths },
    signal,
  });
  const urls: Record<string, string> = {};
  for (const [path, b64] of Object.entries(response.thumbs)) urls[path] = `data:image/jpeg;base64,${b64}`;
  return urls;
}
