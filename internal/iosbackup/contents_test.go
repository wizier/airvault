package iosbackup

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"database/sql"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/objectstore"
)

type fixtureFile struct {
	domain, path string
	flags        int64
	content      []byte
	missing      bool // listed, but its content is not in the snapshot
	listedDelta  int  // the listed size minus the content's: it changed while backed up
}

func fixtureFileID(domain, path string) string {
	sum := sha1.Sum([]byte(domain + "-" + path))
	return hex.EncodeToString(sum[:])
}

// mbFileBlob is a Files.file record shaped like the NSKeyedArchiver output iOS
// writes.
func mbFileBlob(t *testing.T, size int, wrappedKey []byte) []byte {
	t.Helper()
	root := map[string]any{"Size": size, "LastModified": 1_700_000_000, "ProtectionClass": 2, "RelativePath": plist.UID(2)}
	objects := []any{"$null", root, "path"}
	if wrappedKey != nil {
		root["EncryptionKey"] = plist.UID(3)
		objects = append(objects, map[string]any{"NS.data": wrappedKey})
	}
	blob, err := plist.Marshal(map[string]any{
		"$archiver": "NSKeyedArchiver", "$version": 100000,
		"$top": map[string]any{"root": plist.UID(1)}, "$objects": objects,
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

func encryptCBC(t *testing.T, key, plain []byte) []byte {
	t.Helper()
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(bytes.Clone(plain), bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(padded, padded)
	return padded
}

// sqliteFile is the bytes of a database fill builds; a WAL one keeps WAL mode
// in its header, as iOS databases do.
func sqliteFile(t *testing.T, wal bool, fill func(exec func(query string, args ...any))) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if wal {
		exec("PRAGMA journal_mode=WAL")
	}
	fill(exec)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if wal && data[18] != 2 {
		t.Fatal("fixture database is not in WAL mode")
	}
	return data
}

const fixtureSource = "testphoneudid0001"

// buildBackup publishes files as a backup, encrypted with password unless it
// is empty.
func buildBackup(t *testing.T, password string, files []fixtureFile) *Backup {
	t.Helper()
	store, err := objectstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	draft, err := store.BeginSnapshot(fixtureSource, "dddddddd-0000-4000-8000-000000000001", nil)
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, data []byte) {
		writer, err := draft.Create(key)
		if err == nil {
			_, err = writer.Write(data)
		}
		if err == nil {
			err = writer.Commit()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	encrypted := password != ""
	classKey := bytes.Repeat([]byte{0xc1}, 32) // the class 2 key buildKeybag wraps
	wrap := func(key []byte) []byte {
		return append(binary.LittleEndian.AppendUint32(nil, 2), aesWrap(t, classKey, key)...)
	}
	manifestDB := sqliteFile(t, false, func(exec func(string, ...any)) {
		exec("CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)")
		exec("CREATE INDEX FilesDomainIdx ON Files(domain)")
		exec("CREATE INDEX FilesRelativePathIdx ON Files(relativePath)")
		for i, file := range files {
			id := fixtureFileID(file.domain, file.path)
			var wrappedKey []byte
			content := file.content
			if encrypted && file.flags == 1 {
				fileKey := bytes.Repeat([]byte{byte(i + 1)}, 32)
				wrappedKey, content = wrap(fileKey), encryptCBC(t, fileKey, content)
			}
			if file.flags == 1 && !file.missing {
				put(id[:2]+"/"+id, content)
			}
			exec("INSERT INTO Files VALUES (?, ?, ?, ?, ?)", id, file.domain, file.path, file.flags,
				mbFileBlob(t, len(file.content)+file.listedDelta, wrappedKey))
		}
	})
	manifest := map[string]any{"IsEncrypted": encrypted, "Lockdown": map[string]any{"ProductVersion": "18.0"}}
	if encrypted {
		databaseKey := bytes.Repeat([]byte{0xdb}, 32)
		manifest["BackupKeyBag"] = buildKeybag(t, password, true)
		manifest["ManifestKey"] = wrap(databaseKey)
		manifestDB = encryptCBC(t, databaseKey, manifestDB)
	}
	put("Manifest.db", manifestDB)
	for name, value := range map[string]any{"Manifest.plist": manifest, "Status.plist": map[string]any{"SnapshotState": "finished"}} {
		data, err := plist.Marshal(value, plist.XMLFormat)
		if err != nil {
			t.Fatal(err)
		}
		put(name, data)
	}
	if err := WriteInfo(draft, &Info{TargetIdentifier: fixtureSource}); err != nil {
		t.Fatal(err)
	}
	staged, err := draft.Seal(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Publish(staged)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return backup
}

var (
	heicBytes = bytes.Repeat([]byte("photo bytes "), 9)
	jpegBytes = bytes.Repeat([]byte{0xab}, 32)
)

var homeFiles = []fixtureFile{
	{domain: "HomeDomain", path: "Library", flags: 2},
	{domain: "HomeDomain", path: "Library/photo.heic", flags: 1, content: heicBytes},
	{domain: "HomeDomain", path: "Library/aligned.jpg", flags: 1, content: jpegBytes},
	{domain: "HomeDomain", path: "Library/lost", flags: 1, content: []byte("gone"), missing: true},
	{domain: "HomeDomain", path: "Library/shrunk", flags: 1, content: jpegBytes, listedDelta: 4096},
	{domain: "HomeDomain", path: "Library/grown", flags: 1, content: heicBytes, listedDelta: -40},
}

func readAll(t *testing.T, reader Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(io.NewSectionReader(reader, 0, reader.Size()))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

var fixturePasswords = map[string]string{"plain": "", "encrypted": "correct horse"}

func heldComponents(t *testing.T, contents *Contents) []Component {
	t.Helper()
	held, err := contents.Components(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func TestContents(t *testing.T) {
	for name, password := range fixturePasswords {
		t.Run(name, func(t *testing.T) {
			backup := buildBackup(t, password, homeFiles)
			if password != "" {
				if _, err := backup.Unlock(t.Context(), "battery staple"); !errors.Is(err, ErrWrongPassword) {
					t.Fatalf("wrong password: err=%v, want ErrWrongPassword", err)
				}
			}
			contents, err := backup.Unlock(t.Context(), password)
			if err != nil {
				t.Fatal(err)
			}
			defer contents.Close()

			for path, want := range map[string][]byte{"Library/photo.heic": heicBytes, "Library/aligned.jpg": jpegBytes} {
				file, err := contents.stat(t.Context(), "HomeDomain", path)
				if err != nil || !contents.holds(file) || file.modified.Unix() != 1_700_000_000 {
					t.Fatalf("stat(%q) = %+v, %v", path, file, err)
				}
				reader, err := contents.open(file)
				if err != nil {
					t.Fatal(err)
				}
				if got := readAll(t, reader); !bytes.Equal(got, want) {
					t.Fatalf("%s reads %q, want %q", path, got, want)
				}
				// Mid-block offsets, as a resumed download asks for them.
				for _, offset := range []int{0, 5, 16, 21, len(want) - 3} {
					buffer := make([]byte, 40)
					n, err := reader.ReadAt(buffer, int64(offset))
					end := min(offset+40, len(want))
					if !bytes.Equal(buffer[:n], want[offset:end]) || (err != nil) != (end < offset+40) {
						t.Fatalf("%s ReadAt(%d) = %q, %v; want %q", path, offset, buffer[:n], err, want[offset:end])
					}
				}
				_ = reader.Close()
			}

			// What a file holds wins over what the listing recorded.
			for path, want := range map[string][]byte{"Library/shrunk": jpegBytes, "Library/grown": heicBytes} {
				file, err := contents.stat(t.Context(), "HomeDomain", path)
				if err != nil {
					t.Fatal(err)
				}
				reader, err := contents.open(file)
				if err != nil {
					t.Fatalf("open(%s): %v", path, err)
				}
				if got := readAll(t, reader); !bytes.Equal(got, want) {
					t.Fatalf("%s reads %d bytes, want %d", path, len(got), len(want))
				}
			}

			lost, err := contents.stat(t.Context(), "HomeDomain", "Library/lost")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := contents.open(lost); !errors.Is(err, ErrNotStored) {
				t.Fatalf("unstored file: err=%v, want ErrNotStored", err)
			}
			if _, err := contents.stat(t.Context(), "HomeDomain", "Library/nothing"); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("unlisted path: err=%v", err)
			}

			if err := contents.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := contents.stat(t.Context(), "HomeDomain", "Library"); !errors.Is(err, ErrClosed) {
				t.Fatalf("after Close: err=%v, want ErrClosed", err)
			}
		})
	}
}

// coreData is t as Photos.sqlite stores it.
func coreData(t time.Time) float64 { return float64(t.Unix()-coreDataEpoch) + 0.5 }

func TestPhotosWithoutLibrary(t *testing.T) {
	contents, err := buildBackup(t, "", homeFiles).Unlock(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); held == nil || len(held) != 0 {
		t.Fatalf("Components = %#v; want an empty list", held)
	}
	if _, err := contents.Photos(t.Context()); !errors.Is(err, ErrNotStored) {
		t.Fatalf("Photos without Photos.sqlite: err=%v, want ErrNotStored", err)
	}
}

func TestPhotos(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 5, d, 12, 0, 0, 0, time.UTC) }
	library := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec("CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
		exec(`CREATE TABLE ZASSET (Z_PK INTEGER PRIMARY KEY, ZDIRECTORY VARCHAR, ZFILENAME VARCHAR, ZKIND INTEGER,
			ZKINDSUBTYPE INTEGER, ZDATECREATED TIMESTAMP, ZMODIFICATIONDATE TIMESTAMP, ZFAVORITE INTEGER,
			ZHIDDEN INTEGER, ZTRASHEDSTATE INTEGER)`)
		for _, asset := range []struct {
			name                      string
			kind, subtype             int
			taken                     time.Time
			favorite, hidden, trashed int
		}{
			{"IMG_0001.HEIC", 0, 2, day(1), 1, 0, 0},
			{"IMG_0002.JPG", 0, 0, day(3), 0, 1, 0},
			{"IMG_0003.MOV", 1, 0, day(2), 0, 0, 1},
			{"IMG_0004.HEIC", 0, 0, day(4), 0, 0, 0},
		} {
			exec("INSERT INTO ZASSET (ZDIRECTORY, ZFILENAME, ZKIND, ZKINDSUBTYPE, ZDATECREATED, ZFAVORITE, ZHIDDEN, ZTRASHEDSTATE) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
				"DCIM/100APPLE", asset.name, asset.kind, asset.subtype, coreData(asset.taken), asset.favorite, asset.hidden, asset.trashed)
		}
		exec("INSERT INTO ZASSET (ZKIND) VALUES (0)") // no file: skipped
		exec("CREATE TABLE ZADDITIONALASSETATTRIBUTES (Z_PK INTEGER PRIMARY KEY, ZASSET INTEGER, ZTIMEZONEOFFSET INTEGER)")
		exec("INSERT INTO ZADDITIONALASSETATTRIBUTES (ZASSET, ZTIMEZONEOFFSET) VALUES (4, 10800), (1, NULL)")
		exec(`CREATE TABLE ZGENERICALBUM (Z_PK INTEGER PRIMARY KEY, ZKIND INTEGER, ZTITLE VARCHAR,
			ZPARENTFOLDER INTEGER, ZTRASHEDSTATE INTEGER)`)
		exec(`INSERT INTO ZGENERICALBUM VALUES (1, 3999, NULL, NULL, 0), (2, 4000, 'Trips', 1, 0),
			(3, 2, 'Rome', 2, 0), (4, 2, 'Cats', 1, 0), (5, 2, 'Gone', 1, 1)`)
		// The join table's number varies by iOS version; another one shares its shape.
		exec("CREATE TABLE Z_28ASSETS (Z_28ALBUMS INTEGER, Z_3ASSETS INTEGER, Z_FOK_3ASSETS INTEGER)")
		exec("INSERT INTO Z_28ASSETS VALUES (3, 1, 1), (4, 1, 2), (4, 4, 1), (5, 2, 1)")
		exec("CREATE TABLE Z_40ASSETS (Z_40MOMENTS INTEGER, Z_3ASSETS INTEGER)")
	})
	thumbs := "Media/PhotoData/Thumbnails/V2/DCIM/100APPLE/"
	files := []fixtureFile{
		{domain: cameraRoll, path: "Media/PhotoData/Photos.sqlite", flags: 1, content: library},
		// As iOS lists them: an empty WAL with no content stored, and its index.
		{domain: cameraRoll, path: "Media/PhotoData/Photos.sqlite-wal", flags: 1, content: []byte{}, missing: true},
		{domain: cameraRoll, path: "Media/PhotoData/Photos.sqlite-shm", flags: 1, content: make([]byte, 32768)},
		{domain: cameraRoll, path: "Media/DCIM/100APPLE/IMG_0001.HEIC", flags: 1, content: heicBytes},
		{domain: cameraRoll, path: "Media/DCIM/100APPLE/IMG_0001.MOV", flags: 1, content: []byte("live video")},
		{domain: cameraRoll, path: "Media/DCIM/100APPLE/IMG_0002.JPG", flags: 1, content: jpegBytes},
		{domain: cameraRoll, path: "Media/DCIM/100APPLE/IMG_0003.MOV", flags: 1, content: []byte("movie")},
		{domain: cameraRoll, path: thumbs + "IMG_0001.HEIC/5003.JPG", flags: 1, content: []byte("small")},
		{domain: cameraRoll, path: thumbs + "IMG_0001.HEIC/5005.JPG", flags: 1, content: []byte("large")},
		{domain: cameraRoll, path: thumbs + "IMG_0004.HEIC/5005.JPG", flags: 1, content: []byte("cloud")},
	}
	for name, password := range fixturePasswords {
		t.Run(name, func(t *testing.T) {
			contents, err := buildBackup(t, password, files).Unlock(t.Context(), password)
			if err != nil {
				t.Fatal(err)
			}
			defer contents.Close()
			if held := heldComponents(t, contents); len(held) != 1 || held[0] != ComponentPhotos {
				t.Fatalf("Components = %v", held)
			}
			library, err := contents.Photos(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := []Photo{
				{Path: "Media/DCIM/100APPLE/IMG_0004.HEIC", Kind: KindPhoto, Taken: day(4), thumbnail: thumbs + "IMG_0004.HEIC/5005.JPG",
					Albums: []int64{4}},
				{Path: "Media/DCIM/100APPLE/IMG_0002.JPG", Kind: KindPhoto, Taken: day(3), Hidden: true, Stored: true},
				{Path: "Media/DCIM/100APPLE/IMG_0003.MOV", Kind: KindVideo, Taken: day(2), Trashed: true, Stored: true},
				{Path: "Media/DCIM/100APPLE/IMG_0001.HEIC", Kind: KindPhoto, Subtype: SubtypeLive, Taken: day(1), Favorite: true, Stored: true,
					thumbnail: thumbs + "IMG_0001.HEIC/5005.JPG", LiveVideo: "Media/DCIM/100APPLE/IMG_0001.MOV", Albums: []int64{3, 4}},
			}
			if len(library.Photos) != len(want) {
				t.Fatalf("Photos = %+v", library.Photos)
			}
			// Taken in Moscow: the clock there shows 15:00.
			if taken := library.Photos[0].Taken; taken.Hour() != 15 || taken.Format("-07:00") != "+03:00" {
				t.Errorf("photo taken at %v, want 15:00 +03:00", taken)
			}
			for i, photo := range library.Photos {
				photo.Taken, photo.id = photo.Taken.Truncate(time.Second).UTC(), 0
				if !reflect.DeepEqual(photo, want[i]) {
					t.Errorf("photo %d = %+v, want %+v", i, photo, want[i])
				}
			}
			if wantAlbums := []Album{{4, "Cats"}, {3, "Trips / Rome"}}; !reflect.DeepEqual(library.Albums, wantAlbums) {
				t.Errorf("Albums = %+v, want %+v", library.Albums, wantAlbums)
			}
			if photo, ok := library.lookup("Media/DCIM/100APPLE/IMG_0002.JPG"); !ok || !photo.Hidden {
				t.Fatalf("Lookup = %+v, %v", photo, ok)
			}
			if !library.holds("Media/DCIM/100APPLE/IMG_0001.MOV") || library.holds("Media/PhotoData/Photos.sqlite") {
				t.Fatal("Holds must admit the library's files only")
			}
			if thumbnail, err := contents.Thumbnail(t.Context(), library.Photos[3].Path); err != nil || string(thumbnail) != "large" {
				t.Fatalf("Thumbnail = %q, %v", thumbnail, err)
			}
			if _, err := contents.Thumbnail(t.Context(), library.Photos[1].Path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Thumbnail without one: err=%v", err)
			}
			reader, modified, err := contents.OpenFile(t.Context(), ComponentPhotos, library.Photos[3].Path)
			if err != nil || modified.Unix() != 1_700_000_000 || !bytes.Equal(readAll(t, reader), heicBytes) {
				t.Fatalf("OpenFile: %v, %v", modified, err)
			}
			if _, _, err := contents.OpenFile(t.Context(), ComponentPhotos, library.Photos[0].Path); !errors.Is(err, ErrNotStored) {
				t.Fatalf("OpenFile of an iCloud-only photo: err=%v, want ErrNotStored", err)
			}
		})
	}
}

func addressBook(t *testing.T) []byte {
	return sqliteFile(t, true, func(exec func(string, ...any)) {
		exec(`CREATE TABLE ABPerson (ROWID INTEGER PRIMARY KEY AUTOINCREMENT, First TEXT, Last TEXT, Middle TEXT,
			Organization TEXT, JobTitle TEXT, Note TEXT)`)
		exec("CREATE TABLE ABMultiValueLabel (value TEXT, UNIQUE(value))")
		exec(`CREATE TABLE ABMultiValue (UID INTEGER PRIMARY KEY, record_id INTEGER, property INTEGER,
			identifier INTEGER, label INTEGER, value TEXT)`)
		exec("INSERT INTO ABPerson (First, Last, Note) VALUES ('Zoe', 'Adams', 'met at work')")
		exec("INSERT INTO ABPerson (Organization) VALUES ('Acme')")
		exec("INSERT INTO ABPerson (First, Middle, Last, JobTitle) VALUES ('Anna', 'B', 'Cole', 'CTO')")
		exec("INSERT INTO ABPerson (Note) VALUES ('nameless')")
		exec(`INSERT INTO ABMultiValueLabel (value) VALUES ('_$!<Mobile>!$_'), ('work line')`)
		exec(`INSERT INTO ABMultiValue (record_id, property, label, value) VALUES
			(1, 3, 1, '+1 555 0100'), (1, 4, NULL, 'zoe@example.com'), (1, 3, 2, '+1 555 0199'), (3, 22, 1, 'https://x'),
			(3, 3, 1, '8 (916) 123-45-67')`)
	})
}

// contactPhoto is the image contactImagesDB holds for Anna and Zoe.
var contactPhoto = []byte("\xff\xd8\xff\xe0 a photo")

// contactImagesDB holds photos of addressBook's people: Anna's thumbnail is a
// JPEG after a byte; Zoe's is pixels, her whole photo a JPEG; Acme's is pixels alone.
func contactImagesDB(t *testing.T) []byte {
	return sqliteFile(t, true, func(exec func(string, ...any)) {
		exec("CREATE TABLE ABThumbnailImage (record_id INTEGER, format INTEGER, data BLOB)")
		exec("CREATE TABLE ABFullSizeImage (record_id INTEGER, data BLOB)")
		exec("INSERT INTO ABThumbnailImage VALUES (3, 0, ?), (1, 0, x'00112233'), (2, 0, x'00112233')",
			append([]byte{1}, contactPhoto...))
		exec("INSERT INTO ABFullSizeImage VALUES (1, ?)", contactPhoto)
	})
}

func TestContacts(t *testing.T) {
	files := append(slices.Clone(homeFiles), fixtureFile{domain: homeDomain, path: contactsDatabase, flags: 1, content: addressBook(t)},
		fixtureFile{domain: homeDomain, path: contactImages, flags: 1, content: contactImagesDB(t)})
	contents, err := buildBackup(t, "correct horse", files).Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	got, err := contents.Contacts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Contact{
		{ID: 2, Organization: "Acme", ContactID: 2},
		{ID: 3, Name: "Anna B Cole", JobTitle: "CTO", Phones: []LabeledValue{{"Mobile", "8 (916) 123-45-67"}}, ContactID: 3},
		{ID: 1, Name: "Zoe Adams", Note: "met at work",
			Phones: []LabeledValue{{"Mobile", "+1 555 0100"}, {"work line", "+1 555 0199"}},
			Emails: []LabeledValue{{"", "zoe@example.com"}}, ContactID: 1},
		{ID: 4, Note: "nameless"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Contacts =\n%+v\nwant\n%+v", got, want)
	}
	for _, id := range []int64{3, 1} {
		if photo, err := contents.ContactPhoto(t.Context(), id); err != nil || !bytes.Equal(photo, contactPhoto) {
			t.Errorf("ContactPhoto(%d) = %x, %v", id, photo, err)
		}
	}
	for _, id := range []int64{2, 4} {
		if _, err := contents.ContactPhoto(t.Context(), id); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ContactPhoto(%d): err=%v, want fs.ErrNotExist", id, err)
		}
	}
}

func TestCalls(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 6, d, 9, 0, 0, 0, time.UTC) }
	history := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec(`CREATE TABLE ZCALLRECORD (Z_PK INTEGER PRIMARY KEY, ZADDRESS, ZNAME VARCHAR, ZDATE TIMESTAMP,
			ZDURATION FLOAT, ZORIGINATED INTEGER, ZANSWERED INTEGER, ZSERVICE_PROVIDER VARCHAR, ZCALLTYPE INTEGER)`)
		for _, call := range [][]any{
			{"+79161234567", nil, day(1), 65.4, 1, 1, providerPhone, 1},
			{"+4915112345678", "Max", day(2), 30.0, 1, 1, "57T9237FN3.net.whatsapp.WhatsApp", 1},
			{"+4915112345678", "Max", day(2).Add(time.Hour), 10.0, 1, 1, "C67CF9S4VU.ph.telegra.Telegraph", 1},
			{[]byte("ZOE@example.com"), nil, day(3), 120.0, 0, 1, providerFaceTime, callTypeVideo},
			{"+15550100", nil, day(4), 0.0, 0, 0, providerPhone, 1},
			{nil, nil, day(5), 0.0, 0, 0, nil, nil}, // a withheld number
		} {
			call[2] = coreData(call[2].(time.Time))
			exec(`INSERT INTO ZCALLRECORD (ZADDRESS, ZNAME, ZDATE, ZDURATION, ZORIGINATED, ZANSWERED, ZSERVICE_PROVIDER,
				ZCALLTYPE) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, call...)
		}
	})
	files := append(slices.Clone(homeFiles),
		fixtureFile{domain: homeDomain, path: contactsDatabase, flags: 1, content: addressBook(t)},
		fixtureFile{domain: homeDomain, path: contactImages, flags: 1, content: contactImagesDB(t)},
		fixtureFile{domain: homeDomain, path: callsDatabase, flags: 1, content: history})
	backup := buildBackup(t, "correct horse", files)
	// Telegram is installed and names itself; WhatsApp is not.
	metadata, err := plist.Marshal(map[string]string{"bundleDisplayName": "Telegram", "itemName": "Telegram Messenger"},
		plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	backup.Info.Applications = map[string]Application{"ph.telegra.Telegraph": {Metadata: metadata}}
	contents, err := backup.Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); !slices.Equal(held, []Component{ComponentCalls, ComponentContacts}) {
		t.Fatalf("Components = %v", held)
	}
	got, err := contents.Calls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Call{
		{Time: day(5), Service: "phone"},
		{Address: "+15550100", Name: "Zoe Adams", Time: day(4), Service: "phone", ContactID: 1},
		{Address: "ZOE@example.com", Name: "Zoe Adams", Time: day(3), Duration: 120, Answered: true, Service: "facetime", Video: true,
			ContactID: 1},
		{Address: "+4915112345678", Name: "Max", Time: day(2).Add(time.Hour), Duration: 10, Outgoing: true, Answered: true,
			Service: "ph.telegra.Telegraph", App: "Telegram"},
		{Address: "+4915112345678", Name: "Max", Time: day(2), Duration: 30, Outgoing: true, Answered: true, Service: "net.whatsapp.WhatsApp"},
		{Address: "+79161234567", Name: "Anna B Cole", Time: day(1), Duration: 65, Outgoing: true, Answered: true, Service: "phone",
			ContactID: 3},
	}
	for i := range got {
		got[i].Time = got[i].Time.Truncate(time.Second)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Calls =\n%+v\nwant\n%+v", got, want)
	}
}

// typedstream is an attributedBody as Messages archives it, holding text.
func typedstream(text string) []byte {
	body := []byte("\x04\x0bstreamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84\x12NSAttributedString\x00\x84\x84\x08NSObject\x00\x85" +
		"\x92\x84\x84\x84\x08NSString\x01\x94\x84\x01+")
	if len(text) < 0x80 {
		body = append(body, byte(len(text)))
	} else {
		body = binary.LittleEndian.AppendUint16(append(body, 0x81), uint16(len(text)))
	}
	return append(append(body, text...), "\x86\x84\x02iI\x01\x05\x92\x84\x84\x84\x0cNSDictionary"...)
}

func TestMessages(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 7, d, 18, 30, 0, 0, time.UTC) }
	nanos := func(d int) int64 { return at(d).Sub(time.Unix(coreDataEpoch, 0)).Nanoseconds() }
	long := strings.Repeat("Привет, это длинное сообщение 👋 ", 8)
	const photo = "Library/SMS/Attachments/ab/01/GUID1/IMG_1.HEIC"
	sms := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec("CREATE TABLE handle (ROWID INTEGER PRIMARY KEY, id TEXT)")
		exec("CREATE TABLE chat (ROWID INTEGER PRIMARY KEY, chat_identifier TEXT, service_name TEXT, display_name TEXT)")
		exec(`CREATE TABLE message (ROWID INTEGER PRIMARY KEY, text TEXT, attributedBody BLOB, handle_id INTEGER, service TEXT,
			date INTEGER, is_from_me INTEGER, associated_message_type INTEGER, item_type INTEGER)`)
		exec("CREATE TABLE chat_message_join (chat_id INTEGER, message_id INTEGER, message_date INTEGER)")
		exec("CREATE TABLE chat_handle_join (chat_id INTEGER, handle_id INTEGER)")
		exec(`CREATE TABLE attachment (ROWID INTEGER PRIMARY KEY, filename TEXT, mime_type TEXT, transfer_name TEXT,
			total_bytes INTEGER, hide_attachment INTEGER)`)
		exec("CREATE TABLE message_attachment_join (message_id INTEGER, attachment_id INTEGER)")
		// The same person over SMS is another handle and another chat; senders by name are apart.
		exec(`INSERT INTO handle VALUES (1, '+79161234567'), (2, 'zoe@example.com'), (3, '+79161234567'), (4, 'MegaFon'),
			(5, 'DIT_MOS')`)
		// Two groups can share their members and stay apart.
		exec(`INSERT INTO chat VALUES (1, '+79161234567', 'iMessage', NULL), (2, 'chat123', 'iMessage', 'Trip'),
			(3, '+79161234567', 'SMS', NULL), (4, 'chat456', 'iMessage', 'Gift'), (5, 'MegaFon', 'SMS', NULL),
			(6, 'DIT_MOS', 'SMS', NULL)`)
		exec("INSERT INTO chat_handle_join VALUES (1, 1), (2, 1), (2, 2), (3, 3), (4, 1), (4, 2), (5, 4), (6, 5)")
		for _, m := range [][]any{
			{1, 1, "hi", nil, 0, nanos(1), 1, 0, 0},
			{2, 1, nil, typedstream("Loved “hi”"), 1, nanos(2), 0, 2000, 0}, // a reaction
			{3, 1, nil, typedstream(long), 1, nanos(3), 0, 0, 0},
			{4, 2, "\uFFFC\uFFFC", nil, 2, nanos(4), 0, 0, 0},
			{5, 2, nil, nil, 0, nanos(5), 1, 0, 1}, // a group event
			{6, 3, "by sms", nil, 3, nanos(2), 1, 0, 0},
			{7, 4, "a scarf?", nil, 2, nanos(1), 0, 0, 0},
			{8, 5, "balance", nil, 4, nanos(0), 0, 0, 0},
			{9, 6, "a fine", nil, 5, nanos(-1), 0, 0, 0},
		} {
			service := "iMessage"
			if m[1] == 3 || m[1] == 5 || m[1] == 6 {
				service = "SMS"
			}
			exec(`INSERT INTO message (ROWID, text, attributedBody, handle_id, service, date, is_from_me, associated_message_type,
				item_type) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, m[0], m[2], m[3], m[4], service, m[5], m[6], m[7], m[8])
			exec("INSERT INTO chat_message_join VALUES (?, ?, ?)", m[1], m[0], m[5])
		}
		// A message can be in two chats of one conversation.
		exec("INSERT INTO chat_message_join VALUES (3, 1, ?)", nanos(1))
		exec(`INSERT INTO attachment VALUES (1, '~/` + photo + `', 'image/heic', 'IMG_1.HEIC', 12, 0),
			(2, '~/Library/SMS/Attachments/cd/02/GUID2/movie.mov', 'video/quicktime', NULL, 99, 0),
			(3, '~/Library/SMS/Attachments/ef/03/GUID3/payload', NULL, NULL, 1, 1)`)
		exec("INSERT INTO message_attachment_join VALUES (4, 1), (4, 2), (4, 3)")
	})
	files := append(slices.Clone(homeFiles),
		fixtureFile{domain: homeDomain, path: contactsDatabase, flags: 1, content: addressBook(t)},
		fixtureFile{domain: homeDomain, path: messagesDatabase, flags: 1, content: sms},
		fixtureFile{domain: mediaDomain, path: photo, flags: 1, content: heicBytes})
	contents, err := buildBackup(t, "correct horse", files).Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); !slices.Equal(held, []Component{ComponentMessages, ComponentContacts}) {
		t.Fatalf("Components = %v", held)
	}

	chats, err := contents.Chats(t.Context(), ComponentMessages)
	if err != nil {
		t.Fatal(err)
	}
	anna, zoe := Participant{Address: "+79161234567", Name: "Anna B Cole"}, Participant{Address: "zoe@example.com", Name: "Zoe Adams"}
	// Reactions and group events count for nothing.
	wantChats := []Chat{
		{IDs: []int64{2}, Title: "Trip", Participants: []Participant{anna, zoe}, Messages: 1, Last: at(4)},
		// A message counts in each chat it is in.
		{IDs: []int64{1, 3}, Title: "Anna B Cole", Participants: []Participant{anna}, Messages: 4, Last: at(3),
			Snippet: strings.TrimSpace(long)},
		{IDs: []int64{4}, Title: "Gift", Participants: []Participant{anna, zoe}, Messages: 1, Last: at(1), Snippet: "a scarf?"},
		{IDs: []int64{5}, Title: "MegaFon", Participants: []Participant{{Address: "MegaFon"}}, Messages: 1, Last: at(0),
			Snippet: "balance"},
		{IDs: []int64{6}, Title: "DIT_MOS", Participants: []Participant{{Address: "DIT_MOS"}}, Messages: 1, Last: at(-1),
			Snippet: "a fine"},
	}
	if !reflect.DeepEqual(chats, wantChats) {
		t.Fatalf("Chats =\n%+v\nwant\n%+v", chats, wantChats)
	}

	messages, err := contents.Messages(t.Context(), ComponentMessages, []int64{1, 3}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantMessages := []Message{
		{ID: 3, Text: strings.TrimSpace(long), Time: at(3), Sender: "+79161234567", Service: "iMessage"},
		{ID: 6, Text: "by sms", Time: at(2), FromMe: true, Service: "SMS"},
		{ID: 1, Text: "hi", Time: at(1), FromMe: true, Service: "iMessage"},
	}
	if !reflect.DeepEqual(messages, wantMessages) {
		t.Fatalf("Messages =\n%+v\nwant\n%+v", messages, wantMessages)
	}
	if page, err := contents.Messages(t.Context(), ComponentMessages, []int64{1, 3}, 2, 1); err != nil || len(page) != 1 || page[0].ID != 1 {
		t.Fatalf("last page = %+v, %v", page, err)
	}
	// A search ignores case and reads what only the attributed body holds; a reaction says nothing.
	found, err := contents.Search(t.Context(), ComponentMessages, "BY SMS")
	if err != nil || !reflect.DeepEqual(found, []Found{{Chat: 3, Message: Message{ID: 6, Text: "by sms", Time: at(2), FromMe: true, Service: "SMS"}}}) {
		t.Fatalf("Search = %+v, %v", found, err)
	}
	for query, want := range map[string]int64{"ПРИВЕТ": 3, "HI": 1} {
		if found, err := contents.Search(t.Context(), ComponentMessages, query); err != nil || len(found) != 1 || found[0].ID != want {
			t.Errorf("Search(%q) = %+v, %v; want message %d", query, found, err, want)
		}
	}
	// In a chat, a search tells where what it found stands among what the chat shows.
	for query, want := range map[string][]Match{"Hi": {{ID: 1, Offset: 2}}, "ПРИВЕТ": {{3, 0}}, "loved": {}} {
		if matches, err := contents.Matches(t.Context(), ComponentMessages, []int64{1, 3}, query); err != nil ||
			!reflect.DeepEqual(matches, want) {
			t.Errorf("Matches(%q) = %+v, %v; want %+v", query, matches, err, want)
		}
	}

	messages, err = contents.Messages(t.Context(), ComponentMessages, []int64{2}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantAttachments := []Attachment{
		{Path: photo, Name: "IMG_1.HEIC", Size: 12},
		{Path: "Library/SMS/Attachments/cd/02/GUID2/movie.mov", Name: "movie.mov", Size: 99, Missing: true},
	}
	if len(messages) != 1 || messages[0].Text != "" || !reflect.DeepEqual(messages[0].Attachments, wantAttachments) {
		t.Fatalf("group messages = %+v", messages)
	}
	reader, _, err := contents.OpenFile(t.Context(), ComponentMessages, photo)
	if err != nil || !bytes.Equal(readAll(t, reader), heicBytes) {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, _, err := contents.OpenFile(t.Context(), ComponentMessages, "Library/Preferences/x.plist"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenFile outside the attachments: err=%v", err)
	}
}

func TestWhatsApp(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 8, d, 12, 0, 0, 0, time.UTC) }
	varint := func(number int, value uint64) []byte {
		return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(number)<<3), value)
	}
	// Anna's large copy wins over the small; Max has only the small, by his LID.
	const avatar, maxAvatar = "Media/Profile/79161234567-1700000000.jpg", "Media/Profile/999-1700000000.thumb"
	chatStorage := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec(`CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR, ZCONTACTIDENTIFIER VARCHAR,
			ZPARTNERNAME VARCHAR, ZSESSIONTYPE INTEGER, ZREMOVED INTEGER)`)
		exec(`CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZTEXT VARCHAR, ZMESSAGEDATE TIMESTAMP,
			ZISFROMME INTEGER, ZFROMJID VARCHAR, ZGROUPMEMBER INTEGER, ZMESSAGETYPE INTEGER, ZGROUPEVENTTYPE INTEGER,
			ZMEDIAITEM INTEGER, ZMESSAGEINFO INTEGER, ZSORT INTEGER)`)
		exec(`CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, ZMEDIALOCALPATH VARCHAR, ZTITLE VARCHAR,
			ZVCARDSTRING VARCHAR, ZVCARDNAME VARCHAR, ZFILESIZE INTEGER, ZLATITUDE FLOAT, ZLONGITUDE FLOAT, ZMETADATA BLOB)`)
		exec("CREATE TABLE ZWAMESSAGEINFO (Z_PK INTEGER PRIMARY KEY, ZRECEIPTINFO BLOB)")
		exec(`CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMEMBERJID VARCHAR,
			ZCONTACTNAME VARCHAR, ZFIRSTNAME VARCHAR)`)
		exec("CREATE TABLE ZWAPROFILEPUSHNAME (Z_PK INTEGER PRIMARY KEY, ZJID VARCHAR, ZPUSHNAME VARCHAR)")
		// One person under a number and a LID; a group; a status feed, a deleted
		// chat and one with nothing shown are left out.
		exec(`INSERT INTO ZWACHATSESSION VALUES
			(1, '79161234567@s.whatsapp.net', NULL, '+7 916 123-45-67', 0, 0),
			(2, '555@lid', '79161234567@s.whatsapp.net', 'Annie', 0, 0),
			(3, '120363-1@g.us', NULL, 'Climbing', 1, 0),
			(4, '79161234567@status', NULL, NULL, 3, 0),
			(5, '4911@s.whatsapp.net', NULL, 'Gone', 0, 1),
			(6, '4922@s.whatsapp.net', NULL, NULL, 0, 0),
			(7, '0@s.whatsapp.net', NULL, 'WhatsApp', 0, 0)`)
		// WhatsApp's formatted number is no name: Max's own one wins.
		exec(`INSERT INTO ZWAGROUPMEMBER VALUES (1, 3, '4915112345678@s.whatsapp.net', ?, NULL),
			(2, 3, '777@lid', NULL, NULL)`, "\u202a+49 151 1234\u20115678\u202c")
		exec("INSERT INTO ZWAPROFILEPUSHNAME VALUES (1, '999@lid', 'Max')")
		exec(`INSERT INTO ZWAMEDIAITEM VALUES (1, NULL, NULL, NULL, NULL, 0, NULL, NULL, ?),
			(2, NULL, 'Peak', NULL, NULL, 0, 46.5, 8.0, NULL), (3, NULL, NULL, NULL, NULL, 0, NULL, NULL, NULL),
			(4, NULL, 'Please sign', NULL, NULL, 0, NULL, NULL, NULL)`,
			protoBytes(87, protoBytes(1, varint(3, 125))))
		exec("INSERT INTO ZWAMESSAGEINFO VALUES (1, ?)", protoBytes(8, protoBytes(2, []byte("Where?"))))
		for _, m := range [][]any{
			// id, chat, text, day, from me, from, member, type, event, media, info, sort
			{1, 1, "hi", 1, 1, nil, nil, 0, 2, nil, nil, 1},
			{2, 1, nil, 2, 1, nil, nil, 59, 0, 1, nil, 2},                            // a call, 125 s
			{3, 1, nil, 3, 0, "79161234567@s.whatsapp.net", nil, 10, 1, nil, nil, 3}, // a missed voice call
			{4, 1, nil, 0, 0, nil, nil, 10, 2, nil, nil, 0},                          // end-to-end encryption
			{5, 2, "from lid", 4, 0, "555@lid", nil, 0, 2, nil, nil, 1},
			{6, 3, nil, 1, 0, "120363-1@g.us", nil, 6, 12, nil, nil, 1}, // you created the group
			{7, 3, nil, 1, 0, "120363-1@g.us", 1, 6, 2, nil, nil, 2},    // you added Max
			{8, 3, "salut", 2, 0, "120363-1@g.us", 2, 0, 2, nil, nil, 3},
			{9, 3, nil, 3, 0, "120363-1@g.us", 1, 14, 0, nil, nil, 4},        // deleted
			{10, 3, nil, 4, 1, nil, nil, 46, 0, nil, 1, 5},                   // a poll
			{11, 3, nil, 5, 0, "120363-1@g.us", 2, 5, 0, 2, nil, 6},          // a location
			{12, 3, nil, 5, 0, "120363-1@g.us", 1, 66, 0, nil, nil, 7},       // an album link
			{13, 3, "Climbing", 6, 0, "120363-1@g.us", 1, 6, 1, nil, nil, 8}, // Max renamed it
			{14, 6, nil, 7, 0, nil, nil, 66, 0, nil, nil, 1},
			{15, 7, nil, 7, 0, nil, nil, 10, 2, nil, nil, 1}, // WhatsApp's own chat: events only
			// What has nothing to show is left out; media and a place the backup lacks keep their kind.
			{16, 1, " ", 3, 1, nil, nil, 0, 0, nil, nil, 4},
			{17, 1, nil, 3, 0, nil, nil, 10, 999, nil, nil, 5},
			{18, 1, nil, 3, 1, nil, nil, 1, 0, 3, nil, 6},
			{19, 1, nil, 3, 1, nil, nil, 5, 0, 3, nil, 7},
			{20, 1, "Contract.pdf", 3, 1, nil, nil, 8, 0, 4, nil, 8}, // a document: its name, and a caption
		} {
			m[3] = coreData(at(m[3].(int)))
			exec("INSERT INTO ZWAMESSAGE VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", m...)
		}
	})
	contactsV2 := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec("CREATE TABLE ZWAADDRESSBOOKCONTACT (Z_PK INTEGER PRIMARY KEY, ZWHATSAPPID VARCHAR, ZFULLNAME VARCHAR, ZLID VARCHAR)")
		exec("INSERT INTO ZWAADDRESSBOOKCONTACT VALUES (1, '33612345678', 'Jean', '777'), (2, '4915112345678', NULL, '999')")
	})
	callHistory := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec("CREATE TABLE ZWAAGGREGATECALLEVENT (Z_PK INTEGER PRIMARY KEY, ZVIDEO INTEGER)")
		exec(`CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, ZDATE TIMESTAMP, ZDURATION FLOAT, ZGROUPJIDSTRING VARCHAR,
			Z1CALLEVENTS INTEGER)`)
		exec("INSERT INTO ZWAAGGREGATECALLEVENT VALUES (1, 1)")
		exec("INSERT INTO ZWACDCALLEVENT VALUES (1, ?, 125, NULL, 1)", coreData(at(2).Add(-30*time.Second)))
	})
	files := append(slices.Clone(homeFiles),
		fixtureFile{domain: homeDomain, path: contactsDatabase, flags: 1, content: addressBook(t)},
		fixtureFile{domain: whatsAppDomain, path: whatsAppDatabase, flags: 1, content: chatStorage},
		fixtureFile{domain: whatsAppDomain, path: whatsAppContacts, flags: 1, content: contactsV2},
		fixtureFile{domain: whatsAppDomain, path: whatsAppCallLog, flags: 1, content: callHistory},
		fixtureFile{domain: whatsAppDomain, path: avatar, flags: 1, content: jpegBytes},
		fixtureFile{domain: whatsAppDomain, path: strings.TrimSuffix(avatar, ".jpg") + ".thumb", flags: 1, content: jpegBytes},
		fixtureFile{domain: whatsAppDomain, path: maxAvatar, flags: 1, content: jpegBytes})
	contents, err := buildBackup(t, "correct horse", files).Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); !slices.Equal(held, []Component{ComponentWhatsApp, ComponentContacts}) {
		t.Fatalf("Components = %v", held)
	}

	chats, err := contents.Chats(t.Context(), ComponentWhatsApp)
	if err != nil {
		t.Fatal(err)
	}
	// The system contacts name Anna over what her chat recorded.
	anna := Participant{Address: "+79161234567", Name: "Anna B Cole", Avatar: avatar}
	maxP := Participant{Address: "+4915112345678", Name: "~Max", Avatar: maxAvatar}
	jean := Participant{Address: "+33612345678", Name: "Jean"}
	wantChats := []Chat{
		// Its last said is the location: an event and an album link are not.
		{IDs: []int64{3}, Title: "Climbing", Participants: []Participant{maxP, jean}, Last: at(5)},
		{IDs: []int64{2, 1}, Title: "Anna B Cole", Participants: []Participant{anna}, Avatar: avatar, Last: at(4),
			Snippet: "from lid"},
	}
	for i := range chats {
		chats[i].Last = chats[i].Last.Truncate(time.Second)
	}
	if !reflect.DeepEqual(chats, wantChats) {
		t.Fatalf("Chats =\n%+v\nwant\n%+v", chats, wantChats)
	}

	read := func(ids ...int64) []Message {
		t.Helper()
		messages, err := contents.Messages(t.Context(), ComponentWhatsApp, ids, 0, 20)
		if err != nil {
			t.Fatal(err)
		}
		for i := range messages {
			messages[i].Time = messages[i].Time.Truncate(time.Second)
		}
		return messages
	}
	wantMessages := []Message{
		{ID: 20, Text: "Please sign", Time: at(3), FromMe: true, Service: "WhatsApp",
			Attachments: []Attachment{{Name: "Contract.pdf", Missing: true, titled: true}}},
		{ID: 19, Time: at(3), FromMe: true, Service: "WhatsApp", Kind: "location"},
		{ID: 18, Time: at(3), FromMe: true, Service: "WhatsApp", Attachments: []Attachment{{Name: "Photo", Missing: true}}},
		{ID: 3, Time: at(3), Sender: anna.Address, Service: "WhatsApp", Kind: "call", Call: &Call{}},
		{ID: 2, Time: at(2), FromMe: true, Service: "WhatsApp", Kind: "call",
			Call: &Call{Duration: 125, Outgoing: true, Answered: true, Video: true}},
		{ID: 1, Text: "hi", Time: at(1), FromMe: true, Service: "WhatsApp"},
		{ID: 4, Time: at(0), Sender: anna.Address, Service: "WhatsApp", Kind: "event",
			Event: &ChatEvent{Code: "encrypted", Actor: &anna}},
	}
	if got := read(1); !reflect.DeepEqual(got, wantMessages) {
		t.Fatalf("Messages =\n%+v\nwant\n%+v", got, wantMessages)
	}
	if both := read(1, 2); len(both) != 8 || both[0].ID != 5 || both[0].Sender != anna.Address {
		t.Fatalf("a person's chats together = %+v", both)
	}
	group := read(3)
	ids := make([]int64, len(group))
	for i, m := range group {
		ids[i] = m.ID
	}
	switch {
	case !slices.Equal(ids, []int64{13, 11, 10, 9, 8, 7, 6}):
		t.Fatalf("group messages %v", ids)
	case group[0].Event.Code != "renamed" || group[0].Event.Text != "Climbing" || *group[0].Event.Actor != maxP:
		t.Errorf("rename = %+v", group[0].Event)
	case group[1].Kind != "location" || group[1].Location.Name != "Peak":
		t.Errorf("location = %+v", group[1])
	case group[2].Kind != "poll" || group[2].Text != "Where?":
		t.Errorf("poll = %+v", group[2])
	case group[3].Kind != "deleted" || group[3].Sender != maxP.Address:
		t.Errorf("deleted = %+v", group[3])
	case group[4].Text != "salut" || group[4].Sender != jean.Address:
		t.Errorf("message from a LID = %+v", group[4])
	case group[5].Event.Code != "added" || group[5].Event.Actor != nil || !slices.Equal(group[5].Event.Targets, []Participant{maxP}):
		t.Errorf("added = %+v", group[5].Event)
	case group[6].Event.Code != "created" || group[6].Event.Actor != nil:
		t.Errorf("created = %+v", group[6].Event)
	}
	// A search reads places' names and documents' titles too; an event's
	// parameter or the name of media not held says nothing.
	for query, want := range map[string]struct{ chat, id int64 }{"SALUT": {3, 8}, "peak": {3, 11}, "where?": {3, 10},
		"contract": {1, 20}, "sign": {1, 20}, "climbing": {}, "photo": {}} {
		found, err := contents.Search(t.Context(), ComponentWhatsApp, query)
		if err != nil || want.id == 0 && len(found) != 0 ||
			want.id != 0 && (len(found) != 1 || found[0].ID != want.id || found[0].Chat != want.chat) {
			t.Errorf("Search(%q) = %+v, %v; want message %d", query, found, err, want)
		}
	}
	for _, want := range []struct {
		chats   []int64
		query   string
		matches []Match
	}{{[]int64{3}, "SALUT", []Match{{8, 4}}}, {[]int64{3}, "where", []Match{{10, 2}}},
		{[]int64{3}, "climbing", []Match{}}, {[]int64{1, 2}, "hi", []Match{{1, 6}}}} {
		if got, err := contents.Matches(t.Context(), ComponentWhatsApp, want.chats, want.query); err != nil ||
			!reflect.DeepEqual(got, want.matches) {
			t.Errorf("Matches(%v, %q) = %+v, %v; want %+v", want.chats, want.query, got, err, want.matches)
		}
	}
	reader, _, err := contents.OpenFile(t.Context(), ComponentWhatsApp, avatar)
	if err != nil || !bytes.Equal(readAll(t, reader), jpegBytes) {
		t.Fatalf("OpenFile of a profile picture: %v", err)
	}
	if _, _, err := contents.OpenFile(t.Context(), ComponentWhatsApp, whatsAppDatabase); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenFile of the database: err=%v", err)
	}
}

func protoBytes(number int, value []byte) []byte {
	field := binary.AppendUvarint(nil, uint64(number)<<3|2)
	return append(binary.AppendUvarint(field, uint64(len(value))), value...)
}

// noteData is a ZDATA blob as Notes writes it: text, and a run naming each
// attachment its U+FFFCs hold.
func noteData(t *testing.T, text string, attachments ...string) []byte {
	t.Helper()
	varint := func(number int, value uint64) []byte {
		return binary.AppendUvarint(binary.AppendUvarint(nil, uint64(number)<<3), value)
	}
	note := append(protoBytes(2, []byte(text)), protoBytes(5, varint(1, 4))...) // a plain run
	for _, id := range attachments {
		info := append(protoBytes(1, []byte(id)), protoBytes(2, []byte("public.data"))...)
		note = append(note, protoBytes(5, append(varint(1, 1), protoBytes(12, info)...))...)
	}
	document := append(varint(2, 1), protoBytes(3, note)...)
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(protoBytes(2, document)); err != nil || writer.Close() != nil {
		t.Fatal("gzip the note")
	}
	return buffer.Bytes()
}

func TestNotes(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 9, d, 8, 0, 0, 0, time.UTC) }
	store := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec(`CREATE TABLE ZICCLOUDSYNCINGOBJECT (Z_PK INTEGER PRIMARY KEY, ZTITLE1 VARCHAR, ZTITLE2 VARCHAR, ZFOLDER INTEGER,
			ZMODIFICATIONDATE1 TIMESTAMP, ZISPASSWORDPROTECTED INTEGER, ZMARKEDFORDELETION INTEGER, ZIDENTIFIER VARCHAR,
			ZTYPEUTI VARCHAR, ZALTTEXT VARCHAR, ZURLSTRING VARCHAR, ZMEDIA INTEGER, ZFILENAME VARCHAR, ZPARENTATTACHMENT INTEGER)`)
		exec("CREATE TABLE ZICNOTEDATA (Z_PK INTEGER PRIMARY KEY, ZNOTE INTEGER, ZDATA BLOB)")
		exec(`INSERT INTO ZICCLOUDSYNCINGOBJECT (Z_PK, ZTITLE1, ZTITLE2, ZFOLDER, ZMODIFICATIONDATE1, ZISPASSWORDPROTECTED,
			ZMARKEDFORDELETION) VALUES (1, NULL, 'Trips', NULL, NULL, 0, 0), (2, 'Rome', NULL, 1, ?, 0, 0),
			(3, 'Bank', NULL, 1, ?, 1, 0), (4, 'Old', NULL, 1, ?, 0, 1), (5, NULL, NULL, 1, NULL, 0, 0)`,
			coreData(at(1)), coreData(at(2)), coreData(at(3)))
		exec(`INSERT INTO ZICCLOUDSYNCINGOBJECT (Z_PK, ZIDENTIFIER, ZTYPEUTI, ZALTTEXT, ZMEDIA, ZFILENAME) VALUES
			(6, 'ATT-IMG', 'public.jpeg', NULL, 7, NULL), (7, 'MEDIA-1', NULL, NULL, NULL, 'IMG_1.jpeg'),
			(8, 'ATT-TAG', 'com.apple.notes.inlinetextattachment.hashtag', '#rome', NULL, NULL),
			(9, 'ATT-TABLE', 'com.apple.notes.table', NULL, NULL, NULL), (10, 'ATT-DRAW', 'com.apple.drawing.2', NULL, NULL, NULL),
			(11, 'ATT-GONE', 'public.heic', NULL, 12, NULL), (12, 'MEDIA-2', NULL, NULL, NULL, 'IMG_2.HEIC'),
			(13, 'ATT-SCAN', 'com.apple.notes.gallery', NULL, NULL, NULL)`)
		// A scanned document's pages, in the order scanned; the second shows its processed image.
		exec(`INSERT INTO ZICCLOUDSYNCINGOBJECT (Z_PK, ZIDENTIFIER, ZTYPEUTI, ZMEDIA, ZFILENAME, ZPARENTATTACHMENT) VALUES
			(14, 'PAGE-1', 'public.jpeg', 15, NULL, 13), (15, 'MEDIA-3', NULL, NULL, 'scan1.jpg', NULL),
			(16, 'PAGE-2', 'public.jpeg', NULL, NULL, 13)`)
		exec("INSERT INTO ZICNOTEDATA VALUES (1, 2, ?), (2, 3, ?), (3, 4, ?), (4, 5, ?)",
			noteData(t, "Rome\nColosseum \uFFFC at 9 \uFFFC\nПантеон\uFFFC\uFFFC\uFFFC\uFFFC",
				"ATT-IMG", "ATT-TAG", "ATT-TABLE", "ATT-DRAW", "ATT-GONE", "ATT-SCAN"),
			[]byte("encrypted bytes"), noteData(t, "Old"), noteData(t, ""))
	})
	const image, drawing = "Accounts/ACCOUNT/Media/MEDIA-1/1_GENERATION/IMG_1.jpeg", "Accounts/ACCOUNT/FallbackImages/ATT-DRAW.jpg"
	const page1, page2 = "Accounts/ACCOUNT/Media/MEDIA-3/scan1.jpg", "Accounts/ACCOUNT/FallbackImages/PAGE-2.jpg"
	files := append(slices.Clone(homeFiles), fixtureFile{domain: notesDomain, path: notesDatabase, flags: 1, content: store})
	for _, file := range []string{image, drawing, page1, page2} {
		files = append(files, fixtureFile{domain: notesDomain, path: file, flags: 1, content: jpegBytes})
	}
	contents, err := buildBackup(t, "correct horse", files).Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); !slices.Equal(held, []Component{ComponentNotes}) {
		t.Fatalf("Components = %v", held)
	}
	notes, err := contents.Notes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i := range notes {
		notes[i].Modified = notes[i].Modified.Truncate(time.Second)
	}
	want := []Note{
		{ID: 3, Title: "Bank", Folder: "Trips", Modified: at(2), Locked: true},
		{ID: 2, Title: "Rome", Folder: "Trips", Modified: at(1), Text: "Colosseum \uFFFC at 9 #rome\nПантеон\uFFFC\uFFFC\uFFFC\uFFFC",
			Attachments: []Attachment{{Name: "IMG_1.jpeg", Path: image}, {Name: "Table"}, {Name: "ATT-DRAW.jpg", Path: drawing},
				{Name: "IMG_2.HEIC", Missing: true},
				{Name: "Scanned document", Pages: []Attachment{{Name: "scan1.jpg", Path: page1}, {Name: "PAGE-2.jpg", Path: page2}}}}},
	}
	if !reflect.DeepEqual(notes, want) {
		t.Fatalf("Notes =\n%+v\nwant\n%+v", notes, want)
	}
	reader, _, err := contents.OpenFile(t.Context(), ComponentNotes, image)
	if err != nil || !bytes.Equal(readAll(t, reader), jpegBytes) {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, _, err := contents.OpenFile(t.Context(), ComponentNotes, notesDatabase); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenFile of the database: err=%v", err)
	}
}

func TestFiles(t *testing.T) {
	contract := []byte("contract text")
	files := append(slices.Clone(homeFiles),
		fixtureFile{domain: fileProviderDomain, path: "", flags: 2},
		fixtureFile{domain: fileProviderDomain, path: "Other.plist", flags: 1, content: jpegBytes},
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage, flags: 2},
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/notes.txt", flags: 1, content: heicBytes},
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/Архив", flags: 2},
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/link", flags: 4},
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/Docs", flags: 2},
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/Docs/Contract.pdf", flags: 1, content: contract},
		// A folder the backup records only through the file inside it.
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/Trips/Rome.jpg", flags: 1, content: jpegBytes},
		// Listed, but its content never made it into the backup.
		fixtureFile{domain: fileProviderDomain, path: fileProviderStorage + "/gone.dat", flags: 1, content: contract, missing: true},
	)
	contents, err := buildBackup(t, "", files).Unlock(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); !slices.Equal(held, []Component{ComponentFiles}) {
		t.Fatalf("Components = %v", held)
	}
	modified := time.Unix(1_700_000_000, 0)
	for folder, want := range map[string][]Entry{
		"": {{Name: "Docs", Folder: true, Modified: modified},
			{Name: "gone.dat", Size: int64(len(contract)), Modified: modified, Missing: true},
			{Name: "notes.txt", Size: int64(len(heicBytes)), Modified: modified},
			{Name: "Trips", Folder: true}, {Name: "Архив", Folder: true, Modified: modified}},
		"Docs":    {{Name: "Contract.pdf", Size: int64(len(contract)), Modified: modified}},
		"Trips":   {{Name: "Rome.jpg", Size: int64(len(jpegBytes)), Modified: modified}},
		"Архив":   {},
		"Nowhere": {},
	} {
		entries, err := contents.Folder(t.Context(), ComponentFiles, folder)
		if err != nil || !reflect.DeepEqual(entries, want) {
			t.Fatalf("Folder(%q) = %+v, %v; want %+v", folder, entries, err, want)
		}
	}
	reader, _, err := contents.OpenFile(t.Context(), ComponentFiles, "Docs/Contract.pdf")
	if err != nil || !bytes.Equal(readAll(t, reader), contract) {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, _, err := contents.OpenFile(t.Context(), ComponentFiles, "../Other.plist"); err == nil {
		t.Fatal("OpenFile reached out of the folder")
	}
	if _, err := contents.Folder(t.Context(), ComponentPhotos, ""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Folder of the photos: err=%v", err)
	}
}

func TestKeychain(t *testing.T) {
	classKey := bytes.Repeat([]byte{0xc1}, 32) // the class 2 key buildKeybag wraps
	itemKey := bytes.Repeat([]byte{0x5a}, 32)
	itemOfClass := func(class uint32, key []byte, attrs map[string]any) map[string]any {
		plain := kcAttributesDER(t, attrs)
		blob := binary.LittleEndian.AppendUint32(nil, 3) // version
		blob = binary.LittleEndian.AppendUint32(blob, class)
		wrapped := aesWrap(t, key, itemKey)
		blob = binary.LittleEndian.AppendUint32(blob, uint32(len(wrapped)))
		blob = append(blob, wrapped...)
		// CTR is its own inverse, so the decryption seals the fixture too.
		sealed, err := gcmZeroIVDecrypt(itemKey, append(plain, make([]byte, 16)...))
		if err != nil {
			t.Fatal(err)
		}
		blob = append(blob, sealed...)
		blob = append(blob, make([]byte, 16)...) // GCM tag, not checked
		return map[string]any{"v_Data": blob}
	}
	item := func(attrs map[string]any) map[string]any { return itemOfClass(2, classKey, attrs) }
	keychain := map[string]any{
		"genp": []any{
			item(map[string]any{"svce": airPortService, "acct": "Home Wi-Fi", "v_Data": []byte("s3cr3t-wifi")}),
			item(map[string]any{"svce": airPortService, "acct": "Кафе", "v_Data": []byte("latte123")}),
			item(map[string]any{"svce": "SessionToken", "agrp": "ABCDE12345.com.example.app", "acct": "alice", "v_Data": []byte("app-pass")}),
			// An app group serves as a keychain group of its app.
			item(map[string]any{"svce": "sync", "agrp": "group.com.example.app.shared", "acct": "erin", "v_Data": []byte("group-pass")}),
			// An app the backup no longer holds goes by its bundle ID.
			item(map[string]any{"svce": "api", "agrp": "XYZ9876543.org.other.app", "acct": "carol", "v_Data": []byte("other-pass")}),
			item(map[string]any{"svce": "token", "acct": "x", "v_Data": []byte{0xff, 0xfe}}),
			// Wrapped under a class the keybag has no key for, as a non-migratory item is.
			itemOfClass(11, bytes.Repeat([]byte{0x0b}, 32), map[string]any{"svce": airPortService, "acct": "Device Only", "v_Data": []byte("unseen")}),
		},
		"inet": []any{
			item(map[string]any{"srvr": "example.com", "ptcl": "htps", "atyp": "form", "agrp": safariGroup, "acct": "bob", "v_Data": []byte("web-pass")}),
			item(map[string]any{"srvr": "192.168.1.10", "ptcl": "smb ", "port": 445, "atyp": "dflt", "agrp": "com.apple.FileProviderUI.ServerAuthUIExtension", "acct": "me", "v_Data": []byte("smb-pass")}),
			// A group the app shares is the app's; a group of no app the backup holds shows as it is.
			// A protocol may be recorded as the number its four letters make.
			item(map[string]any{"srvr": "nas.local", "ptcl": 0x68747073, "agrp": "ABCDE12345.com.example.app.SwitchAccount", "acct": "admin", "v_Data": []byte("nas-pass")}),
			// An app's own data filed under its bundle ID, with no protocol: a zero.
			item(map[string]any{"srvr": "com.example.app", "ptcl": 0, "atyp": 0, "agrp": "ABCDE12345.com.example.app", "acct": "installId", "v_Data": []byte("install-pass")}),
			item(map[string]any{"srvr": "shared.example", "ptcl": "htps", "agrp": "group.example.shared", "acct": "dave", "v_Data": []byte("shared-pass")}),
			// A key iCloud Keychain syncs with is the system's, not a password.
			item(map[string]any{"srvr": "WiFi", "agrp": "com.apple.security.ckks", "acct": "ACA56A66-E314-4066-B3EE-FB49C0000000", "v_Data": []byte("a2V5LW1hdGVyaWFs")}),
		},
	}
	data, err := plist.Marshal(keychain, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	files := append(slices.Clone(homeFiles), fixtureFile{domain: keychainDomain, path: keychainBackup, flags: 1, content: data})
	contents, err := buildBackup(t, "correct horse", files).Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held := heldComponents(t, contents); !slices.Contains(held, ComponentPasswords) {
		t.Fatalf("Components = %v, want one with passwords", held)
	}
	// An unencrypted backup's keychain is sealed to the device: nothing to show.
	unencrypted, err := buildBackup(t, "", files).Unlock(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer unencrypted.Close()
	if held := heldComponents(t, unencrypted); slices.Contains(held, ComponentPasswords) {
		t.Fatalf("Components of an unencrypted backup = %v", held)
	}
	// An app goes by the name the backup knows it by, found through the item's
	// access group.
	meta, err := plist.Marshal(map[string]any{"bundleDisplayName": "Example"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	icon := []byte("\x89PNG icon")
	contents.backup.Info.Applications = map[string]Application{"com.example.app": {Metadata: meta, PlaceholderIcon: icon}}
	if got, err := contents.AppIcon("com.example.app"); err != nil || !bytes.Equal(got, icon) {
		t.Fatalf("AppIcon = %q, %v", got, err)
	}
	if _, err := contents.AppIcon("org.other.app"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("AppIcon of an app the backup does not hold: err=%v", err)
	}
	secrets, err := contents.Keychain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Secret{
		{ID: 0, Kind: "wifi", Title: "Home Wi-Fi", Password: "s3cr3t-wifi"},
		{ID: 1, Kind: "wifi", Title: "Кафе", Password: "latte123"},
		{ID: 2, Kind: "app", Title: "api", Account: "carol", Password: "other-pass", App: "org.other.app"},
		{ID: 3, Kind: "app", Title: "Example", Account: "alice", Password: "app-pass", App: "Example", BundleID: "com.example.app"},
		{ID: 4, Kind: "app", Title: "Example", Account: "erin", Password: "group-pass", App: "Example", BundleID: "com.example.app"},
		{ID: 5, Kind: "web", Title: "192.168.1.10", Account: "me", Password: "smb-pass", Protocol: "smb", Port: 445,
			App: "Files"},
		{ID: 6, Kind: "web", Title: "Example", Account: "installId", Password: "install-pass",
			App: "Example", BundleID: "com.example.app"},
		{ID: 7, Kind: "web", Title: "example.com", Account: "bob", Password: "web-pass", Protocol: "https", Auth: "form",
			App: "Safari"},
		{ID: 8, Kind: "web", Title: "nas.local", Account: "admin", Password: "nas-pass", Protocol: "https",
			App: "Example", BundleID: "com.example.app"},
		{ID: 9, Kind: "web", Title: "shared.example", Account: "dave", Password: "shared-pass", Protocol: "https",
			App: "group.example.shared"},
	}
	if !reflect.DeepEqual(secrets, want) {
		t.Fatalf("Keychain =\n%+v\nwant\n%+v", secrets, want)
	}
}

// kcAttributesDER encodes keychain item attributes the way iOS does: a DER SET
// of (UTF8String name, value) sequences, data as an OCTET STRING.
func kcAttributesDER(t *testing.T, attrs map[string]any) []byte {
	t.Helper()
	type pair struct {
		Name  string `asn1:"utf8"`
		Value any
	}
	var pairs []pair
	for name, value := range attrs {
		pairs = append(pairs, pair{name, value})
	}
	der, err := asn1.MarshalWithParams(pairs, "set")
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestPersonName(t *testing.T) {
	for recorded, want := range map[string]string{
		"\u202a+49 151 1234\u20115678\u202c": "",
		"(916) 123-45-67":                    "",
		"\u200eAnna Cole\u200f ":             "Anna Cole",
		"❤️":                                 "❤️",
		"Office 2":                           "Office 2",
	} {
		if got := personName(recorded); got != want {
			t.Errorf("personName(%q) = %q, want %q", recorded, got, want)
		}
	}
}

func TestAddressKey(t *testing.T) {
	for _, same := range [][2]string{
		{"+7 916 123-45-67", "8 (916) 123-45-67"},
		{"+41 44 123 45 67", "044 123 45 67"},
		{"+1 555 123 4567", "(555) 123-4567"},
		{"Zoe@Example.com", "zoe@example.com"},
		{"MegaFon", "megafon"},
		{"+7 916 123-45-67 (work)", "8 916 123 45 67"},
	} {
		if addressKey(same[0]) != addressKey(same[1]) {
			t.Errorf("%q and %q do not match", same[0], same[1])
		}
	}
	for _, apart := range [][2]string{
		{"MegaFon", "DIT_MOS"},
		{"Tele2", "Market2"},
		{"900", "+7 916 123-49-00"},
		{"user1234567@example.com", "+1 234 567"},
	} {
		if addressKey(apart[0]) == addressKey(apart[1]) {
			t.Errorf("%q and %q match", apart[0], apart[1])
		}
	}
}

// TestWhatsAppDocument covers how WhatsApp may keep a document's name and
// caption: the name in the text, the caption in the title, or the name alone.
func TestWhatsAppDocument(t *testing.T) {
	var c Contents
	for _, want := range []struct{ text, title, shown, file string }{
		{"Contract.pdf", "Please sign", "Please sign", "Contract.pdf"},
		{"Scan.pdf", "", "", "Scan.pdf"},
		{"", "Invoice.pdf", "", "Invoice.pdf"},
		{"Report.pdf", "Report.pdf", "", "Report.pdf"},
	} {
		m := c.whatsAppMessage(waRow{kind: waDocument, fromMe: true, text: want.text, title: want.title}, &waPeople{}, nil)
		if m.Text != want.shown || len(m.Attachments) != 1 || m.Attachments[0].Name != want.file || !m.Attachments[0].titled {
			t.Errorf("text %q, title %q: %q with %+v", want.text, want.title, m.Text, m.Attachments)
		}
	}
}
