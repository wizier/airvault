package iosbackup

import (
	"context"
	"io"
	"io/fs"
	"strings"
	"time"
)

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
		f.contents.fileErr.Store(&err)
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	read := causeReader{Reader: reader, contents: f.contents}
	return &dbFile{SectionReader: io.NewSectionReader(read, 0, reader.Size()), reader: reader, name: name}, nil
}

// causeReader keeps the cause of a failed database read for the query.
type causeReader struct {
	Reader
	contents *Contents
}

func (r causeReader) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.Reader.ReadAt(p, offset)
	if err != nil && err != io.EOF {
		r.contents.fileErr.Store(&err)
	}
	return n, err
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
