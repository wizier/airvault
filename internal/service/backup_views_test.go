package service

import (
	"errors"
	"testing"

	"github.com/wizier/airvault/internal/domain"
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

func TestUnlockedBackups(t *testing.T) {
	var u unlockedBackups
	defer u.closeIf(func(*unlockedBackup) bool { return true })
	opened := map[string]*iosbackup.Contents{}
	for _, id := range []string{"a", "b", "c"} {
		opened[id] = u.put(id, "phone", &iosbackup.Contents{})
	}
	// A second open of a snapshot keeps the first, and counts as its use.
	if again := u.put("a", "phone", &iosbackup.Contents{}); again != opened["a"] {
		t.Fatal("a second open replaced the first")
	}
	// A fourth closes the one used longest ago.
	u.put("d", "phone", &iosbackup.Contents{})
	for id, want := range map[string]bool{"a": true, "b": false, "c": true, "d": true} {
		if open := u.get(id) != nil; open != want {
			t.Errorf("%s open = %v, want %v", id, open, want)
		}
	}
	u.closeIf(func(b *unlockedBackup) bool { return b.snapshotID == "c" })
	if u.get("c") != nil || u.get("a") != opened["a"] {
		t.Error("closeIf closed the wrong backups")
	}
}

func TestMessageQueriesChecked(t *testing.T) {
	s := &Service{}
	_, chatless := s.BackupChatSearch(t.Context(), "x", iosbackup.ComponentMessages, nil, "hi")
	_, short := s.BackupSearch(t.Context(), "x", iosbackup.ComponentMessages, " a ")
	for code, err := range map[string]error{"chat_required": chatless, "query_too_short": short} {
		if v, ok := errors.AsType[*domain.ValidationError](err); !ok || v.Code != code {
			t.Errorf("%s: err = %v", code, err)
		}
	}
}
