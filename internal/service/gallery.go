package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
)

type GalleryAsset struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind"` // "photo" | "video"
	// LiveVideo is a Live Photo's video, by path like the photo's.
	LiveVideo string `json:"liveVideo,omitempty"`
	// A backup's library knows when a photo was taken, and which originals
	// stayed only in iCloud.
	Taken   *time.Time `json:"taken,omitempty"`
	Missing bool       `json:"missing,omitempty"`
}

const galleryCacheTTL = 5 * time.Minute

// galleryIndex caches only paths and grouping for stable pagination. Thumbnail
// bytes are always read from the phone and are never cached by the server.
type galleryIndex struct {
	mu   sync.Mutex
	byID map[string]galleryEntry
}

type galleryEntry struct {
	assets   []GalleryAsset
	at       time.Time
	revision string
}

func newGalleryIndex() *galleryIndex {
	return &galleryIndex{byID: make(map[string]galleryEntry)}
}

func (g *galleryIndex) revision(udid, revision string) (galleryEntry, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	entry, ok := g.byID[udid]
	expired := ok && time.Since(entry.at) >= galleryCacheTTL
	if !ok || entry.revision != revision || expired {
		if expired {
			delete(g.byID, udid)
		}
		return galleryEntry{}, false
	}
	return entry, true
}

func (g *galleryIndex) put(udid string, entry galleryEntry) {
	g.mu.Lock()
	g.byID[udid] = entry
	g.mu.Unlock()
}

func (g *galleryIndex) remove(udid string) {
	g.mu.Lock()
	delete(g.byID, udid)
	g.mu.Unlock()
}

var imageExts = map[string]bool{".heic": true, ".heif": true, ".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".dng": true}
var videoExts = map[string]bool{".mov": true, ".mp4": true, ".m4v": true}

// An order-sensitive fingerprint that keeps pagination on one scan.
func galleryRevision(assets []GalleryAsset) string {
	digest := sha256.New()
	var length [4]byte
	for _, asset := range assets {
		binary.BigEndian.PutUint32(length[:], uint32(len(asset.Path)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(asset.Path))
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func validPage(offset, limit int) error {
	if offset < 0 || limit < 1 || limit > 500 {
		return &domain.ValidationError{Code: "invalid_page", Message: "offset must be non-negative and limit must be 1-500"}
	}
	return nil
}

func (s *Service) GalleryPage(ctx context.Context, udid string, offset, limit int, revision string) ([]GalleryAsset, int, string, error) {
	if err := validPage(offset, limit); err != nil {
		return nil, 0, "", err
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, 0, "", err
	}
	entry, err := s.galleryAssets(ctx, udid, revision)
	if err != nil {
		return nil, 0, "", err
	}
	total := len(entry.assets)
	offset = min(offset, total)
	return entry.assets[offset:min(offset+limit, total)], total, entry.revision, nil
}

func (s *Service) galleryAssets(ctx context.Context, udid, revision string) (galleryEntry, error) {
	if revision != "" {
		if entry, ok := s.gallery.revision(udid, revision); ok {
			return entry, nil
		}
		return galleryEntry{}, domain.NewActionError(
			"gallery_revision_changed",
			domain.ErrOperationState,
		)
	}
	// A request without a revision starts a new viewing session. It owns its
	// scan and context; one cancelled browser request cannot cancel another.
	assets, err := s.enumerateCameraRoll(ctx, udid)
	if err != nil {
		return galleryEntry{}, newEngineActionError("gallery_failed", err)
	}
	entry := galleryEntry{assets: assets, at: time.Now(), revision: galleryRevision(assets)}
	s.gallery.put(udid, entry)
	return entry, nil
}

// Stats only the handful of DCIM root children, then lists albums names-only:
// never one stat per asset.
func (s *Service) enumerateCameraRoll(ctx context.Context, udid string) ([]GalleryAsset, error) {
	session, release, err := s.openLeasedSession(ctx, udid, devicefs.Media(), deviceReadResource(udid), "gallery_failed")
	if err != nil {
		return nil, err
	}
	defer release()
	dcim, _ := devicefs.ParsePath("DCIM") // a valid constant
	albums, err := session.Children(dcim)
	if err != nil {
		return nil, err
	}
	assets := []GalleryAsset{} // never nil, so an empty camera roll serializes as []
	for _, album := range albums {
		entry, statErr := session.Stat(album)
		if statErr != nil {
			// One odd DCIM child must not sink the whole scan; skip and go on.
			slog.WarnContext(ctx, "gallery: skipping album", "udid", udid, "album", album.Name(), "error", statErr)
			continue
		}
		if entry.Kind != devicefs.EntryDirectory {
			continue
		}
		files, err := session.Children(album)
		if err != nil {
			slog.WarnContext(ctx, "gallery: skipping album", "udid", udid, "album", album.Name(), "error", err)
			continue
		}
		assets = append(assets, groupAlbum(files)...)
	}
	// AFC does not expose capture dates without one stat per asset. Path order is
	// deterministic and usually close to capture order, but is not labelled as
	// chronological in the API.
	slices.SortFunc(assets, func(a, b GalleryAsset) int { return strings.Compare(b.Path, a.Path) })
	return assets, nil
}

// An image with a same-stem .MOV is a Live Photo; a lone .MOV is a video; .AAE
// edit sidecars and everything else are dropped.
func groupAlbum(files []devicefs.Path) []GalleryAsset {
	type item struct{ image, video devicefs.Path }
	byStem := map[string]*item{}
	order := []string{}
	for _, file := range files {
		name := file.Name()
		ext := strings.ToLower(path.Ext(name))
		if !imageExts[ext] && !videoExts[ext] {
			continue
		}
		stem := strings.TrimSuffix(name, path.Ext(name))
		it := byStem[stem]
		if it == nil {
			it = &item{}
			byStem[stem] = it
			order = append(order, stem)
		}
		if imageExts[ext] {
			it.image = file
		} else {
			it.video = file
		}
	}
	out := make([]GalleryAsset, 0, len(order))
	for _, stem := range order {
		it := byStem[stem]
		switch {
		case it.image.String() != "":
			out = append(out, GalleryAsset{
				Path:      it.image.String(),
				Name:      it.image.Name(),
				Kind:      "photo",
				LiveVideo: it.video.String(),
			})
		case it.video.String() != "":
			out = append(out, GalleryAsset{
				Path: it.video.String(),
				Name: it.video.Name(),
				Kind: "video",
			})
		}
	}
	return out
}

// iOS keeps compatibility thumbnails at
// PhotoData/Thumbnails/V2/<dcimPath>/<code>.JPG (tiny JPEGs, videos included).
func readThumbInSession(session *devicefs.Session, dcimPath string) ([]byte, error) {
	assetPath, err := parseRequiredPath(dcimPath)
	if err != nil {
		return nil, err
	}
	thumbDir, err := devicefs.ParsePath("PhotoData/Thumbnails/V2/" + assetPath.String())
	if err != nil {
		return nil, &domain.ValidationError{Code: "invalid_thumbnail_path", Message: "invalid thumbnail path"}
	}
	thumbs, err := session.Children(thumbDir)
	if err != nil {
		return nil, err
	}
	thumb := pickThumb(thumbs)
	if thumb.String() == "" {
		return nil, domain.ErrNotFound
	}
	return session.ReadFile(thumb)
}

// The AFC session died (e.g. a timed-out read closed it) while the request
// itself is still alive.
func sessionDead(ctx context.Context, err error) bool {
	return ctx.Err() == nil && errors.Is(err, io.ErrClosedPipe)
}

// One media session per page: one lockdown handshake instead of one per tile.
func (s *Service) ThumbBatch(ctx context.Context, udid string, dcimPaths []string) (map[string][]byte, error) {
	if len(dcimPaths) == 0 {
		return nil, &domain.ValidationError{Code: "paths_required", Message: "at least one path is required"}
	}
	if len(dcimPaths) > 128 {
		return nil, &domain.ValidationError{Code: "too_many_paths", Message: "too many paths in one batch"}
	}
	return thumbBatch(ctx, func() (*devicefs.Session, func(), error) {
		return s.openLeasedSession(ctx, udid, devicefs.Media(), deviceReadResource(udid), "thumb_failed")
	}, dcimPaths)
}

// Retries once on a fresh session when the transport dies mid-batch.
func thumbBatch(ctx context.Context, open func() (*devicefs.Session, func(), error), dcimPaths []string) (map[string][]byte, error) {
	session, release, err := open()
	if err != nil {
		return nil, err
	}
	// Closing the request (tab/gallery gone) closes the session, aborting an
	// in-flight read.
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer func() { stop(); release() }()
	out := make(map[string][]byte, len(dcimPaths))
	reopened := false
	for i := 0; i < len(dcimPaths); i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		data, err := readThumbInSession(session, dcimPaths[i])
		if err == nil {
			out[dcimPaths[i]] = data
			continue
		}
		if !sessionDead(ctx, err) {
			continue // this asset has no readable thumbnail; the batch goes on
		}
		if reopened {
			break // the transport died twice — return what the batch already has
		}
		reopened = true
		stop()
		release()
		next, nextRelease, openErr := open()
		if openErr != nil {
			return out, nil // device gone mid-batch: partial result
		}
		session, release = next, nextRelease
		stop = context.AfterFunc(ctx, func() { _ = next.Close() })
		i-- // retry the path that observed the dead session
	}
	return out, nil
}

// pickThumb chooses the largest thumbnail JPEG (highest size-code) in a V2 dir.
func pickThumb(thumbs []devicefs.Path) devicefs.Path {
	var best devicefs.Path
	for _, thumb := range thumbs {
		if strings.HasSuffix(strings.ToLower(thumb.Name()), ".jpg") && thumb.Name() > best.Name() {
			best = thumb
		}
	}
	return best
}
