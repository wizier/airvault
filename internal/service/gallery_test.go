package service

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/engine"
)

func TestGalleryIndexIsOnlyAddressableByPageRevision(t *testing.T) {
	index := newGalleryIndex()
	entry := galleryEntry{
		assets:   []GalleryAsset{{Path: "DCIM/100APPLE/IMG_0001.HEIC"}},
		at:       time.Now(),
		revision: "page-session",
	}
	index.put("phone", entry)

	if _, ok := index.revision("phone", ""); ok {
		t.Fatal("an unversioned request must not reuse the gallery index")
	}
	got, ok := index.revision("phone", entry.revision)
	if !ok || len(got.assets) != 1 {
		t.Fatalf("revision lookup = %#v, %v", got, ok)
	}
	if _, ok := index.revision("phone", "another-session"); ok {
		t.Fatal("a different pagination session reused the gallery index")
	}
}

func TestGalleryIndexExpiresPaginationRevision(t *testing.T) {
	index := newGalleryIndex()
	index.put("phone", galleryEntry{
		at:       time.Now().Add(-galleryCacheTTL),
		revision: "expired-session",
	})
	if _, ok := index.revision("phone", "expired-session"); ok {
		t.Fatal("expired gallery pagination revision remained readable")
	}
	if len(index.byID) != 0 {
		t.Fatalf("expired gallery entry was not removed: %#v", index.byID)
	}
}

func mediaPaths(t *testing.T, dir string, names ...string) []devicefs.Path {
	t.Helper()
	paths := make([]devicefs.Path, 0, len(names))
	for _, name := range names {
		path, err := devicefs.ParsePath(dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func TestGroupAlbumRecognizesLivePhotosVideosAndProRAW(t *testing.T) {
	assets := groupAlbum(mediaPaths(t, "DCIM/100APPLE",
		"IMG_0001.HEIC",
		"IMG_0001.MOV",
		"IMG_0002.MOV",
		"IMG_0003.DNG",
		"IMG_0003.AAE",
		"notes.txt",
	))
	if len(assets) != 3 {
		t.Fatalf("assets = %#v, want 3", assets)
	}
	if assets[0].Kind != "photo" || !assets[0].Live || assets[0].Path != "DCIM/100APPLE/IMG_0001.HEIC" {
		t.Fatalf("live photo = %#v", assets[0])
	}
	if assets[1].Kind != "video" || assets[1].Name != "IMG_0002.MOV" {
		t.Fatalf("standalone video = %#v", assets[1])
	}
	if assets[2].Kind != "photo" || assets[2].Name != "IMG_0003.DNG" {
		t.Fatalf("ProRAW photo = %#v", assets[2])
	}
}

func TestPickThumbIsCaseInsensitive(t *testing.T) {
	thumbs := mediaPaths(t, "PhotoData/Thumbnails/V2/DCIM/100APPLE/IMG_0001.HEIC", "5005.jpg", "5008.JPG", "metadata.plist")
	if got := pickThumb(thumbs).Name(); got != "5008.JPG" {
		t.Fatalf("pickThumb = %q", got)
	}
}

// thumbSession serves V2 thumbnail reads and then fails like a closed native
// slot (io.ErrClosedPipe) once okReads is exhausted.
type thumbSession struct {
	okReads int
	reads   int
}

func (s *thumbSession) List(string) ([]string, error)        { return []string{"5005.JPG"}, nil }
func (s *thumbSession) Stat(string) (engine.AFCEntry, error) { return engine.AFCEntry{}, nil }
func (s *thumbSession) Open(string) (engine.AFCFile, error)  { return nil, errors.New("unused") }
func (s *thumbSession) Remove(string) error                  { return nil }
func (s *thumbSession) Close() error                         { return nil }
func (s *thumbSession) ReadSmall(string) ([]byte, error) {
	if s.reads >= s.okReads {
		return nil, io.ErrClosedPipe
	}
	s.reads++
	return []byte("jpeg"), nil
}

func thumbOpen(sessions ...engine.AFCSession) (func() (*devicefs.Session, func(), error), *int) {
	opened := 0
	manager := devicefs.New(openAFCFunc(func() (engine.AFCSession, error) {
		if opened >= len(sessions) {
			return nil, errors.New("no more sessions")
		}
		session := sessions[opened]
		opened++
		return session, nil
	}))
	return func() (*devicefs.Session, func(), error) {
		session, err := manager.Open(context.Background(), "phone", devicefs.Media())
		if err != nil {
			return nil, nil, err
		}
		return session, func() { _ = session.Close() }, nil
	}, &opened
}

type openAFCFunc func() (engine.AFCSession, error)

func (f openAFCFunc) OpenAFC(context.Context, engine.DeviceID, engine.AFCSource, string) (engine.AFCSession, error) {
	return f()
}

func TestThumbBatchReopensDeadSessionOnce(t *testing.T) {
	paths := []string{"DCIM/100APPLE/IMG_0001.HEIC", "DCIM/100APPLE/IMG_0002.HEIC", "DCIM/100APPLE/IMG_0003.HEIC"}

	open, opened := thumbOpen(&thumbSession{okReads: 1}, &thumbSession{okReads: len(paths)})
	out, err := thumbBatch(context.Background(), open, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(paths) {
		t.Fatalf("thumbs = %d, want %d (the path that hit the dead session must be retried)", len(out), len(paths))
	}
	if *opened != 2 {
		t.Fatalf("sessions opened = %d, want 2", *opened)
	}

	// A second transport death ends the batch with the partial result.
	open, opened = thumbOpen(&thumbSession{okReads: 1}, &thumbSession{})
	out, err = thumbBatch(context.Background(), open, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || *opened != 2 {
		t.Fatalf("thumbs = %d, sessions = %d; want the partial result after two deaths", len(out), *opened)
	}
}
