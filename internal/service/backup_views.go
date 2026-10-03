package service

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/iosbackup"
)

// BackupComponents lists the parts of a restore point there are views for.
func (s *Service) BackupComponents(ctx context.Context, snapshotID string) ([]iosbackup.Component, error) {
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	held, err := contents.Components(ctx)
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return held, nil
}

// The photo endpoints take an asset's path as its id: only paths of the
// library resolve, so they read nothing else of the backup.

func (s *Service) backupPhotoLibrary(ctx context.Context, snapshotID string) (*iosbackup.Contents, *iosbackup.PhotoLibrary, error) {
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, nil, err
	}
	library, err := contents.Photos(ctx)
	if err != nil {
		return nil, nil, s.backupError(ctx, contents.Source(), err)
	}
	return contents, library, nil
}

// visible is the library as the Photos app's main view shows it, without
// hidden and recently deleted photos; albums show the same.
func visible(p iosbackup.Photo) bool { return !p.Hidden && !p.Trashed }

// photoFilters are the views of the library in the order the UI shows them:
// the library's own, then the Photos app's media-type albums. A user album is
// "album:<id>".
var photoFilters = []struct {
	key, group string
	shows      func(iosbackup.Photo) bool
}{
	{"", "library", visible},
	{"favorites", "library", func(p iosbackup.Photo) bool { return visible(p) && p.Favorite }},
	{"videos", "library", func(p iosbackup.Photo) bool { return visible(p) && p.Kind == iosbackup.KindVideo }},
	{"live", "library", subtype(iosbackup.SubtypeLive)},
	{"hidden", "library", func(p iosbackup.Photo) bool { return p.Hidden && !p.Trashed }},
	{"deleted", "library", func(p iosbackup.Photo) bool { return p.Trashed }},
	{"screenshots", "media", subtype(iosbackup.SubtypeScreenshot)},
	{"panoramas", "media", subtype(iosbackup.SubtypePanorama)},
	{"slomo", "media", subtype(iosbackup.SubtypeSlowMotion)},
	{"timelapse", "media", subtype(iosbackup.SubtypeTimeLapse)},
	{"screenrecordings", "media", subtype(iosbackup.SubtypeScreenRecording)},
}

func subtype(subtype int) func(iosbackup.Photo) bool {
	return func(p iosbackup.Photo) bool { return visible(p) && p.Subtype == subtype }
}

func photoFilter(key string) (func(iosbackup.Photo) bool, bool) {
	if id, ok := strings.CutPrefix(key, "album:"); ok {
		album, err := strconv.ParseInt(id, 10, 64)
		return func(p iosbackup.Photo) bool { return visible(p) && slices.Contains(p.Albums, album) }, err == nil
	}
	for _, filter := range photoFilters {
		if filter.key == key {
			return filter.shows, true
		}
	}
	return nil, false
}

// filteredPhotos is what filter picks, newest first; month ("2026-10") narrows
// it to one month.
func (s *Service) filteredPhotos(ctx context.Context, snapshotID, filter, month string) ([]iosbackup.Photo, error) {
	shows, ok := photoFilter(filter)
	if !ok {
		return nil, &domain.ValidationError{Code: "invalid_photo_filter", Message: "unknown photo filter"}
	}
	_, library, err := s.backupPhotoLibrary(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	var photos []iosbackup.Photo
	for _, photo := range library.Photos {
		if shows(photo) && (month == "" || photoMonth(photo) == month) {
			photos = append(photos, photo)
		}
	}
	return photos, nil
}

// photoMonth is the month a photo was taken in the server's time zone, "" when
// unknown.
func photoMonth(photo iosbackup.Photo) string {
	if photo.Taken.IsZero() {
		return ""
	}
	return photo.Taken.Local().Format("2006-01")
}

func (s *Service) BackupPhotos(ctx context.Context, snapshotID, filter, month string, offset, limit int) ([]GalleryAsset, int, error) {
	if err := validPage(offset, limit); err != nil {
		return nil, 0, err
	}
	photos, err := s.filteredPhotos(ctx, snapshotID, filter, month)
	if err != nil {
		return nil, 0, err
	}
	offset = min(offset, len(photos))
	page := []GalleryAsset{} // an empty page is [], not null
	for _, photo := range photos[offset:min(offset+limit, len(photos))] {
		page = append(page, galleryAsset(photo))
	}
	return page, len(photos), nil
}

type PhotoMonth struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

// BackupPhotoMonths counts the photos filter picks by month, newest first.
func (s *Service) BackupPhotoMonths(ctx context.Context, snapshotID, filter string) ([]PhotoMonth, error) {
	photos, err := s.filteredPhotos(ctx, snapshotID, filter, "")
	if err != nil {
		return nil, err
	}
	months := []PhotoMonth{}
	for _, photo := range photos { // newest first, so a month's photos are together
		month := photoMonth(photo)
		switch {
		case month == "":
		case len(months) > 0 && months[len(months)-1].Month == month:
			months[len(months)-1].Count++
		default:
			months = append(months, PhotoMonth{Month: month, Count: 1})
		}
	}
	return months, nil
}

type PhotoFilter struct {
	Filter string `json:"filter"`
	Group  string `json:"group"`           // "library" | "media" | "album"
	Title  string `json:"title,omitempty"` // a user album's
	Count  int    `json:"count"`
}

// BackupPhotoFilters lists the filters that show any photo, "" always.
func (s *Service) BackupPhotoFilters(ctx context.Context, snapshotID string) ([]PhotoFilter, error) {
	_, library, err := s.backupPhotoLibrary(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	filters := []PhotoFilter{}
	for _, filter := range photoFilters {
		count := 0
		for _, photo := range library.Photos {
			if filter.shows(photo) {
				count++
			}
		}
		if count > 0 || filter.key == "" {
			filters = append(filters, PhotoFilter{Filter: filter.key, Group: filter.group, Count: count})
		}
	}
	byAlbum := map[int64]int{}
	for _, photo := range library.Photos {
		if visible(photo) {
			for _, album := range photo.Albums {
				byAlbum[album]++
			}
		}
	}
	for _, album := range library.Albums {
		if count := byAlbum[album.ID]; count > 0 {
			filters = append(filters, PhotoFilter{Filter: "album:" + strconv.FormatInt(album.ID, 10), Group: "album",
				Title: album.Title, Count: count})
		}
	}
	return filters, nil
}

func galleryAsset(photo iosbackup.Photo) GalleryAsset {
	asset := GalleryAsset{Path: photo.Path, Name: path.Base(photo.Path), Kind: string(photo.Kind),
		LiveVideo: photo.LiveVideo, Missing: !photo.Stored}
	if !photo.Taken.IsZero() {
		// In the server's zone, the one months are counted in.
		asset.Taken = new(photo.Taken.Local())
	}
	return asset
}

// BackupPhotoThumbs reads iOS's own thumbnails; photos without one are left
// out, as from the phone.
func (s *Service) BackupPhotoThumbs(ctx context.Context, snapshotID string, paths []string) (map[string][]byte, error) {
	if len(paths) == 0 {
		return nil, &domain.ValidationError{Code: "paths_required", Message: "at least one path is required"}
	}
	if len(paths) > 128 {
		return nil, &domain.ValidationError{Code: "too_many_paths", Message: "too many paths in one batch"}
	}
	contents, library, err := s.backupPhotoLibrary(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	thumbs := make(map[string][]byte, len(paths))
	for _, photoPath := range paths {
		photo, ok := library.Lookup(photoPath)
		if !ok {
			continue
		}
		data, err := contents.Thumbnail(ctx, photo)
		switch {
		case err == nil:
			thumbs[photoPath] = data
		case errors.Is(err, iosbackup.ErrClosed), ctx.Err() != nil:
			return nil, s.backupError(ctx, contents.Source(), err)
		case !errors.Is(err, fs.ErrNotExist):
			s.noticeReadDamage(ctx, contents.Source(), err) // this photo goes without; the batch goes on
		}
	}
	return thumbs, nil
}

// OpenBackupPhoto opens a photo or a Live Photo's video.
func (s *Service) OpenBackupPhoto(ctx context.Context, snapshotID, photoPath string) (*BackupFileDownload, error) {
	contents, library, err := s.backupPhotoLibrary(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	if !library.Holds(photoPath) {
		return nil, domain.ErrNotFound
	}
	reader, modified, err := contents.OpenPhoto(ctx, photoPath)
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return s.openBackupDownload(ctx, contents.Source(), reader, modified), nil
}

func (s *Service) BackupChats(ctx context.Context, snapshotID string) ([]iosbackup.Chat, error) {
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	chats, err := contents.Chats(ctx)
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return chats, nil
}

// BackupMessages pages a conversation's messages, the latest first.
func (s *Service) BackupMessages(ctx context.Context, snapshotID string, chatIDs []int64, offset, limit int) ([]iosbackup.Message, error) {
	if err := validPage(offset, limit); err != nil {
		return nil, err
	}
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	messages, err := contents.Messages(ctx, chatIDs, offset, limit)
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return messages, nil
}

// OpenBackupAttachment opens a message attachment; it reads nothing else of
// the backup.
func (s *Service) OpenBackupAttachment(ctx context.Context, snapshotID, attachmentPath string) (*BackupFileDownload, error) {
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	reader, modified, err := contents.OpenAttachment(ctx, attachmentPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return s.openBackupDownload(ctx, contents.Source(), reader, modified), nil
}

func (s *Service) BackupCalls(ctx context.Context, snapshotID string) ([]iosbackup.Call, error) {
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	calls, err := contents.Calls(ctx)
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return calls, nil
}

func (s *Service) BackupContacts(ctx context.Context, snapshotID string) ([]iosbackup.Contact, error) {
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return nil, err
	}
	contacts, err := contents.Contacts(ctx)
	if err != nil {
		return nil, s.backupError(ctx, contents.Source(), err)
	}
	return contacts, nil
}
