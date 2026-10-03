package iosbackup

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"howett.net/plist"
	_ "modernc.org/sqlite"
	"modernc.org/sqlite/vfs"
)

// ErrNotStored is a listed file whose content the backup does not hold.
var ErrNotStored = errors.New("file content is not in the backup")

// ErrClosed is a read after Close.
var ErrClosed = errors.New("backup contents are closed")

// listedFile is a Manifest.db record.
type listedFile struct {
	domain, path string
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
	queries  *gate // Close waits for the queries in flight

	mu        sync.Mutex
	databases map[string]*sql.DB    // by VFS name, opened on first use
	fileErr   atomic.Pointer[error] // the cause of a failed open or read, for cause

	photosMu sync.Mutex // held while the library is read, so it is read once
	photos   *PhotoLibrary

	whatsAppMu sync.Mutex // held while WhatsApp's people are read, likewise
	whatsApp   *waPeople
}

// Unlock opens the backup's file list; an encrypted backup needs its password,
// and a wrong one is ErrWrongPassword.
func (b *Backup) Unlock(ctx context.Context, password string) (*Contents, error) {
	contents := &Contents{backup: b, queries: newGate(), databases: map[string]*sql.DB{}}
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
	if c.manifest == nil || !c.queries.close() { // a zero Contents was never unlocked
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
// file, so a failure shows here with its cause. Large sorts stay in memory:
// the VFS has no temporary files to spill them to.
func (c *Contents) openDatabase(ctx context.Context, name string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+url.PathEscape(name)+"?vfs="+c.vfs+"&mode=ro&immutable=1&_pragma=temp_store(memory)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, err := range c.rows(ctx, db, "SELECT count(*) FROM sqlite_master") {
		if err != nil {
			_ = db.Close()
			return nil, err
		}
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

// rows runs a query and yields its rows; a failure ends them, after Close it
// is ErrClosed.
func (c *Contents) rows(ctx context.Context, db *sql.DB, query string, args ...any) iter.Seq2[*sql.Rows, error] {
	return func(yield func(*sql.Rows, error) bool) {
		if !c.queries.enter() {
			yield(nil, ErrClosed)
			return
		}
		defer c.queries.leave()
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			yield(nil, c.cause(err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			if !yield(rows, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(nil, c.cause(err))
		}
	}
}

// cause adds to a SQLite error the Go error behind it: SQLite reports a failed
// open or read only as CANTOPEN or IOERR.
func (c *Contents) cause(err error) error {
	if cause := c.fileErr.Swap(nil); cause != nil {
		return fmt.Errorf("%w: %w", err, *cause)
	}
	return err
}

// gate admits work until it closes; closing waits for the work admitted.
type gate struct {
	mu     sync.Mutex
	idle   *sync.Cond
	active int
	closed bool
}

func newGate() *gate {
	g := &gate{}
	g.idle = sync.NewCond(&g.mu)
	return g
}

// enter admits one more, unless the gate is closed.
func (g *gate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closed {
		g.active++
	}
	return !g.closed
}

func (g *gate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active--; g.active == 0 {
		g.idle.Broadcast()
	}
}

// close admits no more and waits for those admitted; false when it already was.
func (g *gate) close() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	open := !g.closed
	g.closed = true
	for g.active > 0 {
		g.idle.Wait()
	}
	return open
}

// stat is fs.ErrNotExist for a path the backup does not list.
func (c *Contents) stat(ctx context.Context, domain, path string) (listedFile, error) {
	for rows, err := range c.rows(ctx, c.manifest,
		"SELECT fileID, domain, relativePath, flags, file FROM Files WHERE domain = ? AND relativePath = ?", domain, path) {
		if err != nil {
			return listedFile{}, err
		}
		return scanFile(rows)
	}
	return listedFile{}, fs.ErrNotExist
}

// storedPaths is every regular file of domain the backup lists under prefix.
func (c *Contents) storedPaths(ctx context.Context, domain, prefix string) (map[string]bool, error) {
	paths := map[string]bool{}
	for rows, err := range c.rows(ctx, c.manifest, `SELECT relativePath FROM Files
		WHERE domain = ? AND flags = 1 AND substr(relativePath, 1, length(?)) = ?`, domain, prefix, prefix) {
		var path string
		if err == nil {
			err = rows.Scan(&path)
		}
		if err != nil {
			return nil, err
		}
		paths[path] = true
	}
	return paths, nil
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

// holds reports a file whose content is in the snapshot; a folder has none.
func (c *Contents) holds(file listedFile) bool {
	_, ok := c.backup.FileSize(file.key())
	return ok
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

// Core Data stores time as seconds since 2001-01-01 UTC.
const coreDataEpoch = 978307200

// coreDataTime reads a Core Data timestamp; zero is none.
func coreDataTime(value sql.NullFloat64) time.Time {
	if !value.Valid || value.Float64 == 0 {
		return time.Time{}
	}
	return time.Unix(coreDataEpoch, 0).Add(time.Duration(value.Float64 * float64(time.Second))).UTC()
}

// fileKey is where the snapshot keeps a file the backup lists: its file ID is
// the SHA-1 of "<domain>-<path>".
func fileKey(domain, filePath string) string {
	sum := sha1.Sum([]byte(domain + "-" + filePath))
	id := hex.EncodeToString(sum[:])
	return id[:2] + "/" + id
}

// stored reports a file the backup lists and the snapshot holds.
func (c *Contents) stored(domain, filePath string) bool {
	_, ok := c.backup.FileSize(fileKey(domain, filePath))
	return ok
}
