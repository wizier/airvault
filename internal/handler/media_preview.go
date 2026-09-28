package handler

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"

	"github.com/gen2brain/heic"
)

// Both maps mirror PREVIEW_IMAGE_EXT in web/src/lib/components/PreviewImage.svelte.
var nativeImageType = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

var transcodeImageExt = map[string]bool{".heic": true, ".heif": true}

// Force the embedded WASM decoder (pure Go, no cgo, no external libheif) so
// preview rendering is identical on every host.
func init() { heic.ForceWasmMode = true }

// Orientation is applied by the decoder.
func renderHEICPreview(r io.Reader) ([]byte, error) {
	img, err := heic.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("decode heic: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 88}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}
