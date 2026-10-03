package iosbackup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"database/sql"
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
				if err != nil || !file.regular || file.modified.Unix() != 1_700_000_000 {
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
	if held, err := contents.Components(t.Context()); err != nil || held == nil || len(held) != 0 {
		t.Fatalf("Components = %#v, %v; want an empty list", held, err)
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
			if held, err := contents.Components(t.Context()); err != nil || len(held) != 1 || held[0] != ComponentPhotos {
				t.Fatalf("Components = %v, %v", held, err)
			}
			library, err := contents.Photos(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := []Photo{
				{Path: "Media/DCIM/100APPLE/IMG_0004.HEIC", Kind: KindPhoto, Taken: day(4), Thumbnail: thumbs + "IMG_0004.HEIC/5005.JPG",
					Albums: []int64{4}},
				{Path: "Media/DCIM/100APPLE/IMG_0002.JPG", Kind: KindPhoto, Taken: day(3), Hidden: true, Stored: true},
				{Path: "Media/DCIM/100APPLE/IMG_0003.MOV", Kind: KindVideo, Taken: day(2), Trashed: true, Stored: true},
				{Path: "Media/DCIM/100APPLE/IMG_0001.HEIC", Kind: KindPhoto, Subtype: SubtypeLive, Taken: day(1), Favorite: true, Stored: true,
					Thumbnail: thumbs + "IMG_0001.HEIC/5005.JPG", LiveVideo: "Media/DCIM/100APPLE/IMG_0001.MOV", Albums: []int64{3, 4}},
			}
			if len(library.Photos) != len(want) {
				t.Fatalf("Photos = %+v", library.Photos)
			}
			for i, photo := range library.Photos {
				photo.Taken, photo.id = photo.Taken.Truncate(time.Second), 0
				if !reflect.DeepEqual(photo, want[i]) {
					t.Errorf("photo %d = %+v, want %+v", i, photo, want[i])
				}
			}
			if wantAlbums := []Album{{4, "Cats"}, {3, "Trips / Rome"}}; !reflect.DeepEqual(library.Albums, wantAlbums) {
				t.Errorf("Albums = %+v, want %+v", library.Albums, wantAlbums)
			}
			if photo, ok := library.Lookup("Media/DCIM/100APPLE/IMG_0002.JPG"); !ok || !photo.Hidden {
				t.Fatalf("Lookup = %+v, %v", photo, ok)
			}
			if !library.Holds("Media/DCIM/100APPLE/IMG_0001.MOV") || library.Holds("Media/PhotoData/Photos.sqlite") {
				t.Fatal("Holds must admit the library's files only")
			}
			if thumbnail, err := contents.Thumbnail(t.Context(), library.Photos[3]); err != nil || string(thumbnail) != "large" {
				t.Fatalf("Thumbnail = %q, %v", thumbnail, err)
			}
			if _, err := contents.Thumbnail(t.Context(), library.Photos[1]); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("Thumbnail without one: err=%v", err)
			}
			reader, modified, err := contents.OpenPhoto(t.Context(), library.Photos[3].Path)
			if err != nil || modified.Unix() != 1_700_000_000 || !bytes.Equal(readAll(t, reader), heicBytes) {
				t.Fatalf("OpenPhoto: %v, %v", modified, err)
			}
			if _, _, err := contents.OpenPhoto(t.Context(), library.Photos[0].Path); !errors.Is(err, ErrNotStored) {
				t.Fatalf("OpenPhoto of an iCloud-only photo: err=%v, want ErrNotStored", err)
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

func TestContacts(t *testing.T) {
	files := append(slices.Clone(homeFiles), fixtureFile{domain: homeDomain, path: contactsDatabase, flags: 1, content: addressBook(t)})
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
		{Organization: "Acme"},
		{Name: "Anna B Cole", JobTitle: "CTO", Phones: []LabeledValue{{"Mobile", "8 (916) 123-45-67"}}},
		{Name: "Zoe Adams", Note: "met at work",
			Phones: []LabeledValue{{"Mobile", "+1 555 0100"}, {"work line", "+1 555 0199"}},
			Emails: []LabeledValue{{"", "zoe@example.com"}}},
		{Note: "nameless"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Contacts =\n%+v\nwant\n%+v", got, want)
	}
}

func TestCalls(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 6, d, 9, 0, 0, 0, time.UTC) }
	history := sqliteFile(t, true, func(exec func(string, ...any)) {
		exec(`CREATE TABLE ZCALLRECORD (Z_PK INTEGER PRIMARY KEY, ZADDRESS, ZNAME VARCHAR, ZDATE TIMESTAMP,
			ZDURATION FLOAT, ZORIGINATED INTEGER, ZANSWERED INTEGER, ZSERVICE_PROVIDER VARCHAR, ZCALLTYPE INTEGER)`)
		for _, call := range [][]any{
			{"+79161234567", nil, day(1), 65.4, 1, 1, providerPhone, 1},
			{"+4915112345678", "Max", day(2), 30.0, 1, 1, "net.whatsapp.WhatsApp", 1},
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
		fixtureFile{domain: homeDomain, path: callsDatabase, flags: 1, content: history})
	contents, err := buildBackup(t, "correct horse", files).Unlock(t.Context(), "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	defer contents.Close()
	if held, err := contents.Components(t.Context()); err != nil || !slices.Equal(held, []Component{ComponentContacts, ComponentCalls}) {
		t.Fatalf("Components = %v, %v", held, err)
	}
	got, err := contents.Calls(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []Call{
		{Time: day(5), Service: "phone"},
		{Address: "+15550100", Name: "Zoe Adams", Time: day(4), Service: "phone"},
		{Address: "ZOE@example.com", Name: "Zoe Adams", Time: day(3), Duration: 120, Answered: true, Service: "facetime", Video: true},
		{Address: "+4915112345678", Name: "Max", Time: day(2), Duration: 30, Outgoing: true, Answered: true, Service: "net.whatsapp.WhatsApp"},
		{Address: "+79161234567", Name: "Anna B Cole", Time: day(1), Duration: 65, Outgoing: true, Answered: true, Service: "phone"},
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
		// The same person over SMS is another handle and another chat.
		exec("INSERT INTO handle VALUES (1, '+79161234567'), (2, 'zoe@example.com'), (3, '+79161234567')")
		exec(`INSERT INTO chat VALUES (1, '+79161234567', 'iMessage', NULL), (2, 'chat123', 'iMessage', 'Trip'),
			(3, '+79161234567', 'SMS', NULL)`)
		exec("INSERT INTO chat_handle_join VALUES (1, 1), (2, 1), (2, 2), (3, 3)")
		for _, m := range [][]any{
			{1, 1, "hi", nil, 0, nanos(1), 1, 0, 0},
			{2, 1, nil, typedstream("Loved “hi”"), 1, nanos(2), 0, 2000, 0}, // a reaction
			{3, 1, nil, typedstream(long), 1, nanos(3), 0, 0, 0},
			{4, 2, "\uFFFC\uFFFC", nil, 2, nanos(4), 0, 0, 0},
			{5, 2, nil, nil, 0, nanos(5), 1, 0, 1}, // a group event
			{6, 3, "by sms", nil, 3, nanos(2), 1, 0, 0},
		} {
			service := "iMessage"
			if m[1] == 3 {
				service = "SMS"
			}
			exec(`INSERT INTO message (ROWID, text, attributedBody, handle_id, service, date, is_from_me, associated_message_type,
				item_type) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, m[0], m[2], m[3], m[4], service, m[5], m[6], m[7], m[8])
			exec("INSERT INTO chat_message_join VALUES (?, ?, ?)", m[1], m[0], m[5])
		}
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
	if held, err := contents.Components(t.Context()); err != nil || !slices.Equal(held, []Component{ComponentMessages, ComponentContacts}) {
		t.Fatalf("Components = %v, %v", held, err)
	}

	chats, err := contents.Chats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	anna := Participant{"+79161234567", "Anna B Cole"}
	wantChats := []Chat{
		{IDs: []int64{2}, Title: "Trip", Participants: []Participant{anna, {"zoe@example.com", "Zoe Adams"}}, Messages: 2,
			Last: at(5)},
		{IDs: []int64{1, 3}, Title: "Anna B Cole", Participants: []Participant{anna}, Messages: 4, Last: at(3),
			Snippet: strings.TrimSpace(long)},
	}
	for i := range chats {
		chats[i].name, chats[i].identifier = "", ""
	}
	if !reflect.DeepEqual(chats, wantChats) {
		t.Fatalf("Chats =\n%+v\nwant\n%+v", chats, wantChats)
	}

	messages, err := contents.Messages(t.Context(), []int64{1, 3}, 0, 10)
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
	if page, err := contents.Messages(t.Context(), []int64{1, 3}, 2, 1); err != nil || len(page) != 1 || page[0].ID != 1 {
		t.Fatalf("last page = %+v, %v", page, err)
	}

	messages, err = contents.Messages(t.Context(), []int64{2}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	wantAttachments := []Attachment{
		{Path: photo, Name: "IMG_1.HEIC", Type: "image/heic", Size: 12},
		{Path: "Library/SMS/Attachments/cd/02/GUID2/movie.mov", Name: "movie.mov", Type: "video/quicktime", Size: 99, Missing: true},
	}
	if len(messages) != 1 || messages[0].Text != "" || !reflect.DeepEqual(messages[0].Attachments, wantAttachments) {
		t.Fatalf("group messages = %+v", messages)
	}
	reader, _, err := contents.OpenAttachment(t.Context(), photo)
	if err != nil || !bytes.Equal(readAll(t, reader), heicBytes) {
		t.Fatalf("OpenAttachment: %v", err)
	}
	if _, _, err := contents.OpenAttachment(t.Context(), "Library/Preferences/x.plist"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenAttachment outside the attachments: err=%v", err)
	}
}
