import { clientId } from './client-id';

// Extensions the server can render inline as an <img>; HEIC/HEIF go through a
// server-side transcode. Shared by the file browser and the gallery lightbox.
// Mirrors internal/handler/media_preview.go (nativeImageType + transcodeImageExt).
const PREVIEW_IMAGE_EXT = new Set(['jpg', 'jpeg', 'png', 'gif', 'webp', 'bmp', 'heic', 'heif']);

export function isPreviewableImage(name: string): boolean {
  const ext = name.split('.').pop()?.toLowerCase() ?? '';
  return PREVIEW_IMAGE_EXT.has(ext);
}

// Guards one inline <img> transfer against a stale onload/onerror: begin() mints
// a token, settled() applies a result only while it still matches the current one.
export class PreviewTransfer {
  token = $state('');
  loading = $state(false);
  error = $state(false);

  begin(active = true): void {
    this.error = false;
    this.loading = active;
    this.token = active ? clientId('preview') : '';
  }

  cancel(): void {
    this.token = '';
    this.loading = false;
  }

  settled(token: string, failed: boolean): void {
    if (this.token !== token) return;
    this.loading = false;
    this.error = failed;
  }
}
