package service

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/iosbackup"
)

// readBackup reads from a restore point's contents, answering a failure as
// every view does.
func readBackup[T any](ctx context.Context, s *Service, snapshotID string, read func(*iosbackup.Contents) (T, error)) (T, error) {
	var none T
	contents, err := s.backupContents(ctx, snapshotID)
	if err != nil {
		return none, err
	}
	value, err := read(contents)
	if err != nil {
		return none, s.backupError(ctx, contents.Source(), err)
	}
	return value, nil
}

// BackupComponents lists the parts of a restore point there are views for.
func (s *Service) BackupComponents(ctx context.Context, snapshotID string) ([]iosbackup.Component, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Component, error) { return c.Components(), nil })
}

func (s *Service) backupPhotoLibrary(ctx context.Context, snapshotID string) (*iosbackup.PhotoLibrary, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) (*iosbackup.PhotoLibrary, error) { return c.Photos(ctx) })
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

func subtype(kind int) func(iosbackup.Photo) bool {
	return func(p iosbackup.Photo) bool { return visible(p) && p.Subtype == kind }
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
	library, err := s.backupPhotoLibrary(ctx, snapshotID)
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

// photoMonth is the month a photo was taken, on the clock where it was; ""
// when unknown.
func photoMonth(photo iosbackup.Photo) string {
	if photo.Taken.IsZero() {
		return ""
	}
	return photo.Taken.Format("2006-01")
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
	library, err := s.backupPhotoLibrary(ctx, snapshotID)
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
		asset.Taken = photo.Taken.Format("2006-01-02T15:04:05")
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
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) (map[string][]byte, error) {
		// A library that fails to read is not kept: fail the batch once, not per photo.
		if _, err := c.Photos(ctx); err != nil {
			return nil, err
		}
		thumbs := make(map[string][]byte, len(paths))
		for _, photoPath := range paths {
			data, err := c.Thumbnail(ctx, photoPath)
			switch {
			case err == nil:
				thumbs[photoPath] = data
			case errors.Is(err, iosbackup.ErrClosed), ctx.Err() != nil:
				return nil, err
			case !errors.Is(err, fs.ErrNotExist):
				s.noticeReadDamage(ctx, c.Source(), err) // this photo goes without; the batch goes on
			}
		}
		return thumbs, nil
	})
}

// BackupChats lists the conversations of app: Messages or WhatsApp.
func (s *Service) BackupChats(ctx context.Context, snapshotID string, app iosbackup.Component) ([]iosbackup.Chat, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Chat, error) { return c.Chats(ctx, app) })
}

// BackupMessages pages a conversation's messages, the latest first.
func (s *Service) BackupMessages(ctx context.Context, snapshotID string, app iosbackup.Component, chatIDs []int64,
	offset, limit int) ([]iosbackup.Message, error) {
	if err := cmp.Or(validPage(offset, limit), validChats(chatIDs)); err != nil {
		return nil, err
	}
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Message, error) {
		return c.Messages(ctx, app, chatIDs, offset, limit)
	})
}

// BackupSearch finds an app's messages that say query, the latest first.
func (s *Service) BackupSearch(ctx context.Context, snapshotID string, app iosbackup.Component,
	query string) ([]iosbackup.Found, error) {
	query, err := searchQuery(query)
	if err != nil {
		return nil, err
	}
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Found, error) { return c.Search(ctx, app, query) })
}

// BackupChatSearch finds the messages of a conversation that say query, the
// latest first.
func (s *Service) BackupChatSearch(ctx context.Context, snapshotID string, app iosbackup.Component, chatIDs []int64,
	query string) ([]iosbackup.Match, error) {
	query, err := searchQuery(query)
	if err = cmp.Or(err, validChats(chatIDs)); err != nil {
		return nil, err
	}
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Match, error) {
		return c.Matches(ctx, app, chatIDs, query)
	})
}

// validChats checks a request names a conversation's chats.
func validChats(chatIDs []int64) error {
	if len(chatIDs) == 0 {
		return &domain.ValidationError{Code: "chat_required", Message: "name the conversation's chats (chat=…)"}
	}
	return nil
}

func searchQuery(query string) (string, error) {
	if query = strings.TrimSpace(query); utf8.RuneCountInString(query) < 2 {
		return "", &domain.ValidationError{Code: "query_too_short", Message: "search for two characters or more"}
	}
	return query, nil
}

// OpenBackupFile opens a file of a component: a photo or a Live Photo's video,
// or what a message or a note carries. It reads nothing else of the backup.
func (s *Service) OpenBackupFile(ctx context.Context, snapshotID string, component iosbackup.Component,
	filePath string) (*BackupFileDownload, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) (*BackupFileDownload, error) {
		reader, modified, err := c.OpenFile(ctx, component, filePath)
		if err != nil {
			return nil, err
		}
		return s.openBackupDownload(ctx, c.Source(), reader, modified), nil
	})
}

func (s *Service) BackupCalls(ctx context.Context, snapshotID string) ([]iosbackup.Call, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Call, error) { return c.Calls(ctx) })
}

func (s *Service) BackupNotes(ctx context.Context, snapshotID string) ([]iosbackup.Note, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Note, error) { return c.Notes(ctx) })
}

func (s *Service) BackupContacts(ctx context.Context, snapshotID string) ([]iosbackup.Contact, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]iosbackup.Contact, error) { return c.Contacts(ctx) })
}

func (s *Service) BackupContactPhoto(ctx context.Context, snapshotID string, contactID int64) ([]byte, error) {
	return readBackup(ctx, s, snapshotID, func(c *iosbackup.Contents) ([]byte, error) { return c.ContactPhoto(ctx, contactID) })
}
