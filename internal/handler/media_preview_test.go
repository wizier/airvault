package handler

import "testing"

func TestClassifyPreview(t *testing.T) {
	cases := map[string]previewKind{
		"IMG_0001.HEIC":  previewImageTranscode,
		"photo.heif":     previewImageTranscode,
		"screenshot.PNG": previewImageNative,
		"pic.jpg":        previewImageNative,
		"clip.MOV":       previewNone, // no pure-Go video decode → Save-to-view
		"notes.txt":      previewNone,
		"noext":          previewNone,
	}
	for name, want := range cases {
		if got := classifyPreview(name); got != want {
			t.Errorf("classifyPreview(%q) = %d, want %d", name, got, want)
		}
	}
}
