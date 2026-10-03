package iosbackup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"howett.net/plist"
	_ "modernc.org/sqlite"
	"modernc.org/sqlite/vfs"

	"github.com/wizier/airvault/internal/objectstore"
)

// ErrNotStored is a listed file whose content the backup does not hold.
var ErrNotStored = errors.New("file content is not in the backup")

// ErrClosed is a read after Close.
var ErrClosed = errors.New("backup contents are closed")

// listedFile is a Manifest.db record.
type listedFile struct {
	domain, path string
	regular      bool
	modified     time.Time // zero when the backup never recorded it
	id           string
	wrappedKey   []byte // encrypted backups only
}

// manifestDB names Manifest.db to the VFS; every other name is
// "<domain>/<path in the domain>".
const manifestDB = "Manifest.db"

// Contents is an unlocked backup. SQLite reads its databases, Manifest.db
// first, straight from the snapshot through a VFS that decrypts the pages it
// asks for; nothing decrypted is written to disk.
type Contents struct {
	backup   *Backup
	keys     classKeys // nil when the backup is not encrypted
	fsys     *vfs.FS
	vfs      string
	manifest *sql.DB
	closed   atomic.Bool

	mu        sync.Mutex
	databases map[string]*sql.DB // by VFS name, opened on first use
	// SQLite reports a failed open only as CANTOPEN; the cause waits here.
	openErrs sync.Map // VFS name -> error

	photosMu sync.Mutex // held while the library is read, so it is read once
	photos   *PhotoLibrary
}

// Unlock opens the backup's file list; an encrypted backup needs its password,
// and a wrong one is ErrWrongPassword.
func (b *Backup) Unlock(ctx context.Context, password string) (*Contents, error) {
	contents := &Contents{backup: b, databases: map[string]*sql.DB{}}
	if b.Encrypted {
		var err error
		if contents.keys, err = b.unlockKeys(password); err != nil {
			return nil, err
		}
	}
	var err error
	if contents.vfs, contents.fsys, err = vfs.New(backupFS{contents}); err != nil {
		return nil, err
	}
	if contents.manifest, err = contents.openDatabase(ctx, manifestDB); err != nil {
		_ = contents.fsys.Close()
		return nil, fmt.Errorf("open Manifest.db: %w", err)
	}
	return contents, nil
}

func (c *Contents) Source() string { return c.backup.Source() }

func (c *Contents) Close() error {
	if c.closed.Swap(true) || c.manifest == nil { // a zero Contents was never unlocked
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	errs := []error{c.manifest.Close()}
	for _, db := range c.databases {
		errs = append(errs, db.Close())
	}
	return errors.Join(append(errs, c.fsys.Close())...)
}

// openDatabase keeps one connection for the life of the Contents: a new one
// would verify its file from the start again. Reading the schema opens the
// file, so a failure shows here with its cause.
func (c *Contents) openDatabase(ctx context.Context, name string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(name)+"?vfs="+c.vfs+"&mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := c.query(ctx, db, nil, "SELECT 1 FROM sqlite_master WHERE 0"); err != nil {
		_ = db.Close()
		if cause, ok := c.openErrs.LoadAndDelete(name); ok {
			err = fmt.Errorf("%w: %w", err, cause.(error))
		}
		return nil, err
	}
	return db, nil
}

// domainDatabase opens a database the backup holds, once.
func (c *Contents) domainDatabase(ctx context.Context, domain, path string) (*sql.DB, error) {
	name := domain + "/" + path
	c.mu.Lock()
	defer c.mu.Unlock()
	if db := c.databases[name]; db != nil {
		return db, nil
	}
	db, err := c.openDatabase(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	c.databases[name] = db
	return db, nil
}

func (c *Contents) query(ctx context.Context, db *sql.DB, scan func(*sql.Rows) error, query string, args ...any) error {
	if c.closed.Load() {
		return ErrClosed
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// stat is fs.ErrNotExist for a path the backup does not list.
func (c *Contents) stat(ctx context.Context, domain, path string) (listedFile, error) {
	var file listedFile
	found := false
	err := c.query(ctx, c.manifest, func(rows *sql.Rows) error {
		var err error
		file, err = scanFile(rows)
		found = true
		return err
	}, "SELECT fileID, domain, relativePath, flags, file FROM Files WHERE domain = ? AND relativePath = ?", domain, path)
	if err == nil && !found {
		err = fs.ErrNotExist
	}
	return file, err
}

// storedPaths is every regular file of domain the backup lists.
func (c *Contents) storedPaths(ctx context.Context, domain string) (map[string]bool, error) {
	paths := map[string]bool{}
	err := c.query(ctx, c.manifest, func(rows *sql.Rows) error {
		var path string
		err := rows.Scan(&path)
		paths[path] = true
		return err
	}, "SELECT relativePath FROM Files WHERE domain = ? AND flags = 1", domain)
	return paths, err
}

func scanFile(rows *sql.Rows) (listedFile, error) {
	var file listedFile
	var flags int64
	var blob []byte
	if err := rows.Scan(&file.id, &file.domain, &file.path, &flags, &blob); err != nil {
		return listedFile{}, err
	}
	if len(file.id) != 40 {
		return listedFile{}, fmt.Errorf("invalid file id %q in Manifest.db", file.id)
	}
	file.regular = flags == 1
	record, err := parseMBFile(blob)
	if err != nil {
		return listedFile{}, fmt.Errorf("%s/%s: %w", file.domain, file.path, err)
	}
	file.wrappedKey = record.wrappedKey
	if record.modified > 0 {
		file.modified = time.Unix(record.modified, 0)
	}
	return file, nil
}

type mbFile struct {
	modified   int64
	wrappedKey []byte
}

// parseMBFile reads Files.file: an NSKeyedArchiver-encoded MBFile.
func parseMBFile(blob []byte) (mbFile, error) {
	var archive struct {
		Objects []any                `plist:"$objects"`
		Top     map[string]plist.UID `plist:"$top"`
	}
	if _, err := plist.Unmarshal(blob, &archive); err != nil {
		return mbFile{}, fmt.Errorf("decode file record: %w", err)
	}
	object := func(uid plist.UID) any {
		if uid < plist.UID(len(archive.Objects)) {
			return archive.Objects[uid]
		}
		return nil
	}
	root, ok := object(archive.Top["root"]).(map[string]any)
	if !ok {
		return mbFile{}, errors.New("file record has no root object")
	}
	record := mbFile{modified: plistInt(root["LastModified"])}
	if uid, ok := root["EncryptionKey"].(plist.UID); ok {
		data, _ := object(uid).(map[string]any)
		record.wrappedKey, _ = data["NS.data"].([]byte)
	}
	return record, nil
}

func plistInt(value any) int64 {
	switch v := value.(type) {
	case uint64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// Reader is a file's content, decrypted, readable at any offset.
type Reader interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// holds reports a regular file whose content is in the snapshot.
func (c *Contents) holds(file listedFile) bool {
	_, ok := c.backup.FileSize(file.key())
	return file.regular && ok
}

func (f listedFile) key() string { return f.id[:2] + "/" + f.id }

func (c *Contents) open(file listedFile) (Reader, error) {
	if !c.holds(file) {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotStored, file.domain, file.path)
	}
	if c.keys != nil && len(file.wrappedKey) == 0 {
		return nil, fmt.Errorf("%s/%s: encrypted file has no key", file.domain, file.path)
	}
	reader, err := c.openStored(file.key(), file.wrappedKey)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", file.domain, file.path, err)
	}
	return reader, nil
}

// openStored opens a stored object, decrypted when it has a key.
func (c *Contents) openStored(key string, wrappedKey []byte) (Reader, error) {
	stored, err := c.backup.OpenRandom(key)
	if err != nil {
		return nil, err
	}
	if len(wrappedKey) == 0 {
		return stored, nil
	}
	decrypted, err := newCBCFile(stored, c.keys, wrappedKey)
	if err != nil {
		_ = stored.Close()
		return nil, err
	}
	return decrypted, nil
}

// openPath opens a file the backup lists, with when it was last modified; one
// it does not hold is ErrNotStored.
func (c *Contents) openPath(ctx context.Context, domain, path string) (Reader, time.Time, error) {
	file, err := c.stat(ctx, domain, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, time.Time{}, fmt.Errorf("%w: %s/%s", ErrNotStored, domain, path)
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	reader, err := c.open(file)
	return reader, file.modified, err
}

// backupFS serves SQLite the backup's databases.
type backupFS struct{ contents *Contents }

func (f backupFS) Open(name string) (fs.File, error) {
	var reader Reader
	var err error
	if name == manifestDB {
		// Its own key is absent before iOS 10.2, when it was not encrypted.
		reader, err = f.contents.openStored(manifestDB, f.contents.backup.manifestKey)
	} else {
		reader, err = f.contents.openListed(name)
	}
	if err != nil {
		f.contents.openErrs.Store(name, err)
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &dbFile{SectionReader: io.NewSectionReader(reader, 0, reader.Size()), reader: reader, name: name}, nil
}

func (c *Contents) openListed(name string) (Reader, error) {
	domain, path, ok := strings.Cut(name, "/")
	if !ok {
		return nil, fs.ErrNotExist
	}
	// SQLite opens files with no context of its own; this bounds the lookup.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reader, _, err := c.openPath(ctx, domain, path)
	return reader, err
}

type dbFile struct {
	*io.SectionReader
	reader Reader
	name   string
}

func (f *dbFile) Stat() (fs.FileInfo, error) { return dbFileInfo{f.name, f.Size()}, nil }

func (f *dbFile) Close() error { return f.reader.Close() }

type dbFileInfo struct {
	name string
	size int64
}

func (i dbFileInfo) Name() string       { return i.name }
func (i dbFileInfo) Size() int64        { return i.size }
func (i dbFileInfo) Mode() fs.FileMode  { return 0o444 }
func (i dbFileInfo) ModTime() time.Time { return time.Time{} }
func (i dbFileInfo) IsDir() bool        { return false }
func (i dbFileInfo) Sys() any           { return nil }

// cbcFile decrypts content stored as AES-256-CBC with a zero IV at any offset:
// plaintext block i is D(C[i]) xor C[i-1].
type cbcFile struct {
	stored *objectstore.File
	block  cipher.Block
	size   int64
}

// The plaintext ends where the PKCS#7 padding of the last block says: the size
// Manifest.db lists is stale for content that changed while it was backed up.
func newCBCFile(stored *objectstore.File, keys classKeys, wrappedKey []byte) (*cbcFile, error) {
	key, err := keys.unwrap(wrappedKey)
	if err != nil {
		return nil, fmt.Errorf("unwrap file key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if stored.Size() == 0 || stored.Size()%aes.BlockSize != 0 {
		return nil, fmt.Errorf("stored %d bytes are not whole AES blocks", stored.Size())
	}
	file := &cbcFile{stored: stored, block: block, size: stored.Size()}
	last := make([]byte, aes.BlockSize)
	if _, err := file.ReadAt(last, file.size-aes.BlockSize); err != nil {
		return nil, err
	}
	pad := int(last[aes.BlockSize-1])
	if pad < 1 || pad > aes.BlockSize || !bytes.Equal(last[aes.BlockSize-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		return nil, errors.New("encrypted content has no valid padding")
	}
	file.size -= int64(pad)
	return file, nil
}

func (f *cbcFile) Size() int64 { return f.size }

func (f *cbcFile) Close() error { return f.stored.Close() }

func (f *cbcFile) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= f.size {
		return 0, io.EOF
	}
	end := min(offset+int64(len(p)), f.size)
	first := offset / aes.BlockSize * aes.BlockSize
	last := (end + aes.BlockSize - 1) / aes.BlockSize * aes.BlockSize
	if end == f.size {
		last = f.stored.Size() // the padding too, so the stored object is verified
	}
	from := max(first-aes.BlockSize, 0)
	buffer := make([]byte, last-from)
	if _, err := f.stored.ReadAt(buffer, from); err != nil {
		return 0, err
	}
	iv, blocks := make([]byte, aes.BlockSize), buffer
	if first > 0 {
		iv, blocks = buffer[:aes.BlockSize], buffer[aes.BlockSize:]
	}
	cipher.NewCBCDecrypter(f.block, iv).CryptBlocks(blocks, blocks)
	n := copy(p, blocks[offset-first:end-first])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
