package service

import (
	"testing"

	"github.com/wizier/airvault/internal/iosbackup"
)

func TestPhotoFilter(t *testing.T) {
	inAlbum := iosbackup.Photo{Albums: []int64{7}, Subtype: iosbackup.SubtypeScreenshot}
	hiddenInAlbum := iosbackup.Photo{Albums: []int64{7}, Hidden: true}
	for _, c := range []struct {
		filter                  string
		ok                      bool
		showsAlbum, showsHidden bool
	}{
		{"album:7", true, true, false},
		{"album:8", true, false, false},
		{"screenshots", true, true, false},
		{"hidden", true, false, true},
		{"album:x", false, false, false},
		{"nope", false, false, false},
	} {
		shows, ok := photoFilter(c.filter)
		if ok != c.ok || ok && (shows(inAlbum) != c.showsAlbum || shows(hiddenInAlbum) != c.showsHidden) {
			t.Errorf("photoFilter(%q): ok=%v", c.filter, ok)
		}
	}
}
