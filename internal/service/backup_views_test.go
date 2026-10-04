package service

import (
	"errors"
	"strconv"
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
	changes := 0
	u := unlockedBackups{changed: func(string) { changes++ }}
	defer u.closeIf(func(*unlockedBackup) bool { return true })
	ids := make([]string, maxUnlocked)
	opened := map[string]*iosbackup.Contents{}
	for i := range ids {
		ids[i] = strconv.Itoa(i)
		opened[ids[i]] = u.put(ids[i], "phone", &iosbackup.Contents{})
	}
	// A second open of a snapshot keeps the first, and counts as its use.
	if again := u.put(ids[0], "phone", &iosbackup.Contents{}); again != opened[ids[0]] {
		t.Fatal("a second open replaced the first")
	}
	// One more closes the one used longest ago.
	u.put("extra", "phone", &iosbackup.Contents{})
	for _, id := range append(ids, "extra") {
		if open, want := u.get(id) != nil, id != ids[1]; open != want {
			t.Errorf("%s open = %v, want %v", id, open, want)
		}
	}
	u.closeIf(func(b *unlockedBackup) bool { return b.snapshotID == ids[2] })
	if u.get(ids[2]) != nil || u.get(ids[0]) != opened[ids[0]] {
		t.Error("closeIf closed the wrong backups")
	}
	// Each open and close counts once; the repeated open changed nothing.
	if want := maxUnlocked + 1 + 2; changes != want {
		t.Errorf("changes = %d, want %d", changes, want)
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
