package iosbackup

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"
	"time"
)

const (
	cameraRoll     = "CameraRollDomain"
	photosDatabase = "Media/PhotoData/Photos.sqlite"
	// iOS's own small JPEGs: <thumbnails>/<asset path under Media>/<size code>.JPG.
	thumbnailsRoot = "Media/PhotoData/Thumbnails/V2/"
)

type PhotoKind string

const (
	KindPhoto PhotoKind = "photo"
	KindVideo PhotoKind = "video"
)

// ZASSET.ZKINDSUBTYPE values the views pick by.
const (
	SubtypePanorama        = 1
	SubtypeLive            = 2
	SubtypeScreenshot      = 10
	SubtypeSlowMotion      = 101
	SubtypeTimeLapse       = 102
	SubtypeScreenRecording = 103
)

// Photo is an asset of the photo library, by its path in CameraRollDomain.
type Photo struct {
	Path      string
	Kind      PhotoKind
	Subtype   int
	Taken     time.Time
	Favorite  bool
	Hidden    bool
	Trashed   bool    // in Recently Deleted
	Stored    bool    // the original is in the backup, not only in iCloud
	LiveVideo string  // the Live Photo's video in the backup, "" when there is none
	Albums    []int64 // the user albums it is in
	id        int64
	thumbnail string // iOS's own in the backup, "" when there is none
}

// Album is a user album, titled under its folders: "Trips / 2025".
type Album struct {
	ID    int64
	Title string
}

// PhotoLibrary is the photo library, newest first, and its albums by title.
type PhotoLibrary struct {
	Photos []Photo
	Albums []Album
	byPath map[string]int
	videos map[string]bool // Live Photos' videos
}

func (l *PhotoLibrary) lookup(path string) (Photo, bool) {
	i, ok := l.byPath[path]
	if !ok {
		return Photo{}, false
	}
	return l.Photos[i], true
}

// holds reports a file of the library: a photo or a Live Photo's video.
func (l *PhotoLibrary) holds(path string) bool {
	_, ok := l.byPath[path]
	return ok || l.videos[path]
}

// Photos reads the library once; the snapshot never changes.
func (c *Contents) Photos(ctx context.Context) (*PhotoLibrary, error) {
	c.photosMu.Lock()
	defer c.photosMu.Unlock()
	if c.photos != nil {
		return c.photos, nil
	}
	// Every request waiting on the lock needs this read: one cancelled must not end it.
	ctx = context.WithoutCancel(ctx)
	db, err := c.domainDatabase(ctx, cameraRoll, photosDatabase)
	if err != nil {
		return nil, err
	}
	photos, err := c.readAssets(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("read the photo library: %w", err)
	}
	stored, err := c.storedPaths(ctx, cameraRoll, "")
	if err != nil {
		return nil, err
	}
	thumbnails := largestThumbnails(stored)
	// Albums are a convenience: the photos stand without them.
	albums, members, err := c.readAlbums(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "photo albums unreadable", "error", err)
	}
	slog.InfoContext(ctx, "photo library read", "photos", len(photos), "albums", len(albums))
	library := &PhotoLibrary{Photos: photos, Albums: albums, byPath: make(map[string]int, len(photos)), videos: map[string]bool{}}
	for i := range library.Photos {
		photo := &library.Photos[i]
		photo.Stored = stored[photo.Path]
		photo.thumbnail = thumbnails[strings.TrimPrefix(photo.Path, "Media/")]
		photo.Albums = members[photo.id]
		// iOS keeps a Live Photo's video beside it, under the same name.
		if video := strings.TrimSuffix(photo.Path, path.Ext(photo.Path)) + ".MOV"; photo.Subtype == SubtypeLive && stored[video] {
			photo.LiveVideo = video
			library.videos[video] = true
		}
		library.byPath[photo.Path] = i
	}
	c.photos = library
	return library, nil
}

func (c *Contents) readAssets(ctx context.Context, db *sql.DB) ([]Photo, error) {
	// Time zones are a convenience: without them a photo is dated in UTC.
	zones, err := c.readTimeZones(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "photo time zones unreadable", "error", err)
	}
	var photos []Photo
	for rows, err := range c.rows(ctx, db, `SELECT Z_PK, ZDIRECTORY, ZFILENAME, ZKIND, ZKINDSUBTYPE, ZDATECREATED, ZFAVORITE,
		ZHIDDEN, ZTRASHEDSTATE FROM ZASSET WHERE ZDIRECTORY IS NOT NULL AND ZFILENAME IS NOT NULL`) {
		var id int64
		var directory, name string
		var kind int
		var subtype, favorite, hidden, trashed sql.NullInt64
		var taken sql.NullFloat64
		if err == nil {
			err = rows.Scan(&id, &directory, &name, &kind, &subtype, &taken, &favorite, &hidden, &trashed)
		}
		if err != nil {
			return nil, err
		}
		photo := Photo{
			id:       id,
			Path:     path.Join("Media", directory, name),
			Kind:     KindPhoto,
			Subtype:  int(subtype.Int64),
			Taken:    coreDataTime(taken),
			Favorite: favorite.Int64 != 0,
			Hidden:   hidden.Int64 != 0,
			Trashed:  trashed.Int64 != 0,
		}
		if kind == 1 {
			photo.Kind = KindVideo
		}
		if zone, ok := zones[id]; ok {
			photo.Taken = photo.Taken.In(zone)
		}
		photos = append(photos, photo)
	}
	slices.SortStableFunc(photos, func(a, b Photo) int {
		return cmp.Or(b.Taken.Compare(a.Taken), cmp.Compare(a.Path, b.Path))
	})
	return photos, nil
}

// readTimeZones maps assets to the time zone they were taken in; one without
// is UTC.
func (c *Contents) readTimeZones(ctx context.Context, db *sql.DB) (map[int64]*time.Location, error) {
	zones := map[int64]*time.Location{}
	for rows, err := range c.rows(ctx, db,
		"SELECT ZASSET, ZTIMEZONEOFFSET FROM ZADDITIONALASSETATTRIBUTES WHERE ZTIMEZONEOFFSET IS NOT NULL") {
		var asset int64
		var offset int
		if err == nil {
			err = rows.Scan(&asset, &offset)
		}
		if err != nil {
			return nil, err
		}
		zones[asset] = time.FixedZone("", offset)
	}
	return zones, nil
}

// ZGENERICALBUM.ZKIND values.
const (
	albumKind  = 2
	folderKind = 4000
)

// readAlbums reads the user albums, titled under their folders, and the albums
// each asset is in.
func (c *Contents) readAlbums(ctx context.Context, db *sql.DB) ([]Album, map[int64][]int64, error) {
	type node struct {
		title  string
		kind   int
		parent int64
	}
	nodes := map[int64]node{}
	for rows, err := range c.rows(ctx, db, `SELECT Z_PK, ZKIND, COALESCE(ZTITLE, ''), COALESCE(ZPARENTFOLDER, 0)
		FROM ZGENERICALBUM WHERE ZKIND IN (?, ?) AND COALESCE(ZTRASHEDSTATE, 0) = 0`, albumKind, folderKind) {
		var id int64
		var n node
		if err == nil {
			err = rows.Scan(&id, &n.kind, &n.title, &n.parent)
		}
		if err != nil {
			return nil, nil, err
		}
		nodes[id] = n
	}
	var albums []Album
	for id, n := range nodes {
		if n.kind != albumKind {
			continue
		}
		title := n.title
		for parent, depth := nodes[n.parent], 0; parent.kind == folderKind && depth < 16; parent, depth = nodes[parent.parent], depth+1 {
			title = parent.title + " / " + title
		}
		albums = append(albums, Album{ID: id, Title: title})
	}
	slices.SortFunc(albums, func(a, b Album) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title)), cmp.Compare(a.ID, b.ID))
	})
	table, albumColumn, assetColumn, err := c.albumMembership(ctx, db)
	if err != nil {
		return albums, nil, err
	}
	members := map[int64][]int64{}
	for rows, err := range c.rows(ctx, db, fmt.Sprintf("SELECT %s, %s FROM %s", albumColumn, assetColumn, table)) {
		var album, asset int64
		if err == nil {
			err = rows.Scan(&album, &asset)
		}
		if err != nil {
			return albums, nil, err
		}
		if nodes[album].kind == albumKind {
			members[asset] = append(members[asset], album)
		}
	}
	return albums, members, nil
}

// albumMembership finds the album-asset join table: Core Data names it
// Z_<n>ASSETS, n set by the iOS version, so it is the one with a Z_<n>ALBUMS
// column beside the Z_<m>ASSETS one.
func (c *Contents) albumMembership(ctx context.Context, db *sql.DB) (table, albumColumn, assetColumn string, err error) {
	columns := map[string][]string{}
	for rows, err := range c.rows(ctx, db, `SELECT m.name, p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name GLOB 'Z_[0-9]*ASSETS'`) {
		var table, column string
		if err == nil {
			err = rows.Scan(&table, &column)
		}
		if err != nil {
			return "", "", "", err
		}
		columns[table] = append(columns[table], column)
	}
	for _, table := range slices.Sorted(maps.Keys(columns)) {
		albumColumn, assetColumn = "", ""
		for _, column := range columns[table] {
			switch {
			case strings.HasPrefix(column, "Z_FOK_"):
			case strings.HasSuffix(column, "ALBUMS"):
				albumColumn = column
			case strings.HasSuffix(column, "ASSETS"):
				assetColumn = column
			}
		}
		if albumColumn != "" && assetColumn != "" {
			return table, albumColumn, assetColumn, nil
		}
	}
	return "", "", "", errors.New("no album membership table")
}

// largestThumbnails maps an asset path under Media to its largest thumbnail
// (highest size code).
func largestThumbnails(stored map[string]bool) map[string]string {
	thumbnails := map[string]string{}
	for file := range stored {
		asset, ok := strings.CutPrefix(file, thumbnailsRoot)
		if !ok || !strings.EqualFold(path.Ext(file), ".jpg") {
			continue
		}
		asset = path.Dir(asset)
		if current := thumbnails[asset]; current == "" || path.Base(file) > path.Base(current) {
			thumbnails[asset] = file
		}
	}
	return thumbnails
}

// Thumbnail reads iOS's thumbnail of a photo; fs.ErrNotExist when there is none.
func (c *Contents) Thumbnail(ctx context.Context, photoPath string) ([]byte, error) {
	library, err := c.Photos(ctx)
	if err != nil {
		return nil, err
	}
	photo, ok := library.lookup(photoPath)
	if !ok || photo.thumbnail == "" {
		return nil, fs.ErrNotExist
	}
	reader, _, err := c.openPath(ctx, cameraRoll, photo.thumbnail)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.NewSectionReader(reader, 0, reader.Size()))
}
