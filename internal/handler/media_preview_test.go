package handler

import "testing"

func TestClassifyPreview(t *testing.T) {
	cases := map[string]previewKind{
		"IMG_0001.HEIC":  previewImageTranscode,
		"photo.heif":     previewImageTranscode,
		"screenshot.PNG": previewImageNative,
		"pic.jpg":        previewImageNative,
		"pic.jpeg":       previewImageNative,
		"anim.gif":       previewImageNative,
		"img.webp":       previewImageNative,
		"clip.MOV":       previewNone, // no pure-Go video decode → Save-to-view
		"clip.mp4":       previewNone,
		"notes.txt":      previewNone,
		"archive.zip":    previewNone,
		"noext":          previewNone,
	}
	for name, want := range cases {
		if got := classifyPreview(name); got != want {
			t.Errorf("classifyPreview(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestNativeImageContentType(t *testing.T) {
	if got := nativeImageContentType("a.PNG"); got != "image/png" {
		t.Errorf("PNG content type = %q, want image/png", got)
	}
	if got := nativeImageContentType("a.heic"); got != "application/octet-stream" {
		t.Errorf("non-native content type = %q, want application/octet-stream", got)
	}
}
