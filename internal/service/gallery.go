package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wizier/airvault/internal/devicefs"
	"github.com/wizier/airvault/internal/domain"
)

// GalleryAsset is one camera-roll item addressed by its media-partition path.
// Live marks a photo that has a paired .MOV (Live Photo).
type GalleryAsset struct {
	Path string `json:"path"` // e.g. "DCIM/100APPLE/IMG_0049.HEIC"
	Name string `json:"name"`
	Kind string `json:"kind"` // "photo" | "video"
	Live bool   `json:"live,omitempty"`
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

func assetPaths(assets []GalleryAsset) []string {
	paths := make([]string, len(assets))
	for i, asset := range assets {
		paths[i] = asset.Path
	}
	return paths
}

// GalleryPage returns a stable page from one indexed camera-roll revision.
func (s *Service) GalleryPage(ctx context.Context, udid string, offset, limit int, revision string) ([]GalleryAsset, int, string, error) {
	if offset < 0 || limit < 1 || limit > 500 {
		return nil, 0, "", &domain.ValidationError{Code: "invalid_gallery_page", Message: "offset must be non-negative and limit must be 1-500"}
	}
	if err := s.reachableDevice(ctx, udid); err != nil {
		return nil, 0, "", err
	}
	entry, err := s.galleryAssets(ctx, udid, revision)
	if err != nil {
		return nil, 0, "", err
	}
	assets := entry.assets
	total := len(assets)
	if offset > total {
		offset = total
	}
	end := total
	if limit < total-offset {
		end = offset + limit
	}
	return assets[offset:end], total, entry.revision, nil
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
	entry := galleryEntry{assets: assets, at: time.Now(), revision: devicefs.ListingRevision(assetPaths(assets))}
	s.gallery.put(udid, entry)
	return entry, nil
}

// enumerateCameraRoll stats only the handful of DCIM root children to identify
// albums, then lists every album names-only (never one stat per asset). Results
// are grouped in deterministic descending path order.
func (s *Service) enumerateCameraRoll(ctx context.Context, udid string) ([]GalleryAsset, error) {
	session, release, err := s.openLeasedSession(ctx, udid, devicefs.Media(), resourceRead, "gallery_failed")
	if err != nil {
		return nil, err
	}
	defer release()
	dcim, err := devicefs.ParsePath("DCIM")
	if err != nil {
		return nil, err
	}
	albumNames, err := session.Names(dcim)
	if err != nil {
		return nil, err
	}
	var assets []GalleryAsset
	for _, album := range albumNames {
		albumPath, pathErr := dcim.Child(album)
		if pathErr != nil {
			return nil, fmt.Errorf("gallery album path %q: %w", album, pathErr)
		}
		entry, statErr := session.Stat(albumPath)
		if statErr != nil {
			// One odd DCIM child must not sink the whole scan; skip and go on.
			slog.WarnContext(ctx, "gallery: skipping album", "udid", udid, "album", album, "error", statErr)
			continue
		}
		if entry.Kind != devicefs.EntryDirectory {
			continue
		}
		names, err := session.Names(albumPath)
		if err != nil {
			slog.WarnContext(ctx, "gallery: skipping album", "udid", udid, "album", album, "error", err)
			continue
		}
		assets = append(assets, groupAlbum(album, names)...)
	}
	// AFC does not expose capture dates without one stat per asset. Path order is
	// deterministic and usually close to capture order, but is not labelled as
	// chronological in the API.
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path > assets[j].Path })
	return assets, nil
}

// groupAlbum turns a DCIM album's raw filenames into assets: an image with a
// same-stem .MOV is a Live Photo; a lone .MOV is a video; .AAE edit sidecars
// and everything else are dropped.
func groupAlbum(album string, names []string) []GalleryAsset {
	type item struct{ image, video string }
	byStem := map[string]*item{}
	order := []string{}
	for _, name := range names {
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
			it.image = name
		} else {
			it.video = name
		}
	}
	out := make([]GalleryAsset, 0, len(order))
	for _, stem := range order {
		it := byStem[stem]
		switch {
		case it.image != "":
			out = append(out, GalleryAsset{
				Path: "DCIM/" + album + "/" + it.image,
				Name: it.image,
				Kind: "photo",
				Live: it.video != "",
			})
		case it.video != "":
			out = append(out, GalleryAsset{
				Path: "DCIM/" + album + "/" + it.video,
				Name: it.video,
				Kind: "video",
			})
		}
	}
	return out
}

// MediaStat returns one media file's size and modified time (a single device
// stat) — e.g. to show a photo's date and size when it is opened.
func (s *Service) MediaStat(ctx context.Context, udid, rawPath string) (devicefs.Entry, error) {
	devicePath, err := parseRequiredPath(rawPath)
	if err != nil {
		return devicefs.Entry{}, err
	}
	session, release, err := s.openLeasedSession(ctx, udid, devicefs.Media(), resourceRead, "stat_failed")
	if err != nil {
		return devicefs.Entry{}, err
	}
	defer release()
	entry, err := session.Stat(devicePath)
	if err != nil {
		return devicefs.Entry{}, newEngineActionError("stat_failed", err)
	}
	if entry.Kind != devicefs.EntryFile {
		return devicefs.Entry{}, &domain.ValidationError{Code: "media_file_required", Message: "path must identify a media file"}
	}
	return entry, nil
}

// readThumbInSession resolves and reads one compatibility thumbnail on an
// already-open media session, so a batch reuses one session. iOS keeps them at
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
	names, err := session.Names(thumbDir)
	if err != nil {
		return nil, err
	}
	code := pickThumb(names)
	if code == "" {
		return nil, domain.ErrNotFound
	}
	thumbPath, err := thumbDir.Child(code)
	if err != nil {
		return nil, domain.ErrNotFound
	}
	return session.ReadFile(thumbPath)
}

// sessionDead reports a transport-level failure: the AFC session died (e.g. a
// timed-out read closed it) while the request itself is still alive.
func sessionDead(ctx context.Context, err error) bool {
	return ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, io.ErrClosedPipe))
}

// ThumbBatch reads a whole gallery page's thumbnails on one media session — one
// lockdown handshake instead of one per tile. Missing thumbnails are omitted.
func (s *Service) ThumbBatch(ctx context.Context, udid string, dcimPaths []string) (map[string][]byte, error) {
	if len(dcimPaths) == 0 {
		return map[string][]byte{}, nil
	}
	return thumbBatch(ctx, func() (*devicefs.Session, func(), error) {
		return s.openLeasedSession(ctx, udid, devicefs.Media(), resourceRead, "thumb_failed")
	}, dcimPaths)
}

// thumbBatch drains dcimPaths on one session from open, retrying once on a
// fresh session when the transport dies mid-batch.
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
func pickThumb(names []string) string {
	best := ""
	for _, n := range names {
		if strings.HasSuffix(strings.ToLower(n), ".jpg") && n > best {
			best = n
		}
	}
	return best
}
