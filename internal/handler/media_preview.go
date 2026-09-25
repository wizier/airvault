package handler

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"path"
	"strings"

	"github.com/gen2brain/heic"
)

// previewKind decides how a media file is rendered inline: native images stream
// as-is, HEIC/HEIF transcode to JPEG, everything else has no inline preview.
type previewKind int

const (
	previewNone previewKind = iota
	previewImageNative
	previewImageTranscode
)

// Browser-renderable image types stream unchanged, keyed to their MIME type.
// Both maps mirror PREVIEW_IMAGE_EXT in web/src/lib/components/PreviewImage.svelte.
var nativeImageType = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
}

// Types the browser can't render but the pure-Go HEIC decoder can.
var transcodeImageExt = map[string]bool{".heic": true, ".heif": true}

// Force the embedded WASM decoder (pure Go, no cgo, no external libheif) so
// preview rendering is identical on every host.
func init() { heic.ForceWasmMode = true }

func classifyPreview(name string) previewKind {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case nativeImageType[ext] != "":
		return previewImageNative
	case transcodeImageExt[ext]:
		return previewImageTranscode
	default:
		return previewNone
	}
}

// nativeImageContentType is the MIME type for a browser-renderable image.
func nativeImageContentType(name string) string {
	if ct := nativeImageType[strings.ToLower(path.Ext(name))]; ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// renderHEICPreview decodes a HEIC/HEIF stream and returns it as JPEG bytes.
// Pure Go: gen2brain/heic runs a HEIC decoder compiled to WASM via wazero — no
// cgo, no external binary. Orientation is applied by the decoder.
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
