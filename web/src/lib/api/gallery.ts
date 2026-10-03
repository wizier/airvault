import { devicePath, request } from './client';
import { endpointFileSource, type FileSource } from './files';

export interface GalleryAsset {
  path: string;
  name: string;
  kind: 'photo' | 'video';
  /** A Live Photo's video, by path like the photo's. */
  liveVideo?: string;
  /** When it was taken; only a backup's library knows. */
  taken?: string;
  /** The original stayed in iCloud and is not in the backup. */
  missing?: boolean;
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
  /** One of the gallery's filters; a backup's library has them. */
  filter?: string;
  /** "2026-10": the photos taken that month. */
  month?: string;
  signal?: AbortSignal;
}

/** A camera roll — the phone's own, or a backup's photo library — served as
 *  base/gallery pages, base/thumbs batches and files by path under base. */
export interface GallerySource {
  page(options: GalleryPageOptions): Promise<GalleryPage>;
  /** Paths with no thumbnail are absent from the result. */
  thumbs(paths: string[], signal?: AbortSignal): Promise<Record<string, string>>;
  files: FileSource;
  /** The roll's filters with their counts; only a backup's library has them. */
  filters?(signal?: AbortSignal): Promise<GalleryFilter[]>;
  /** Photo counts by month for a filter, newest first; only a backup knows dates. */
  months?(filter: string, signal?: AbortSignal): Promise<GalleryMonth[]>;
}

export interface GalleryFilter {
  /** '' is the whole roll. */
  key: string;
  label: string;
  count: number;
  /** The picker's option group; a filter without one is a chip. */
  group?: string;
}

export interface GalleryMonth {
  /** "2026-10" */
  month: string;
  count: number;
}

interface ThumbBatchResponse {
  thumbs: Record<string, string>; // path -> base64 JPEG
}

export function gallerySource(base: string): GallerySource {
  return {
    page: (options) => {
      const q = new URLSearchParams({ offset: String(options.offset), limit: String(options.limit) });
      if (options.revision) q.set('revision', options.revision);
      if (options.filter) q.set('filter', options.filter);
      if (options.month) q.set('month', options.month);
      return request<GalleryPage>(`${base}/gallery?${q}`, { signal: options.signal });
    },
    thumbs: async (paths, signal) => {
      const response = await request<ThumbBatchResponse>(`${base}/thumbs`, { method: 'POST', body: { paths }, signal });
      const urls: Record<string, string> = {};
      for (const [path, b64] of Object.entries(response.thumbs)) urls[path] = `data:image/jpeg;base64,${b64}`;
      return urls;
    },
    files: endpointFileSource(base),
  };
}

export function deviceGallerySource(udid: string): GallerySource {
  return gallerySource(`${devicePath(udid)}/media`);
}
