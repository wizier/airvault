package objectstore

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"sync"
	"time"
)

const tarBlockSize = 512

// Tar is read straight from the objects, nothing staged on disk. Its layout is
// fixed up front, so any byte range can be read and a cut download resumes by
// Range.
type Tar struct {
	*io.SectionReader
	view  *View
	root  string
	paths []string // "" (the root folder) first, then sorted: parents precede children
	// offsets[i] is where entry i (header, content, padding) starts; the extra
	// last one is where the two-block trailer starts.
	offsets []int64

	mu   sync.Mutex // Close may race a read when the client disconnects
	file *tarFile
}

// Content read in order from its start is checked, so a damaged object fails
// the read instead of passing as a complete copy.
type tarFile struct {
	index int
	file  *os.File
	check *contentCheck
}

func (v *View) Tar(root string) (*Tar, error) {
	t := &Tar{view: v, root: root,
		paths: append([]string{""}, slices.Sorted(maps.Keys(v.manifest.Entries))...)}
	t.offsets = make([]int64, 0, len(t.paths)+1)
	var offset int64
	var encoded bytes.Buffer
	for _, logicalPath := range t.paths {
		t.offsets = append(t.offsets, offset)
		header := t.header(logicalPath)
		encoded.Reset()
		if err := tar.NewWriter(&encoded).WriteHeader(header); err != nil {
			return nil, err
		}
		offset += int64(encoded.Len()) + header.Size + tarPadding(header.Size)
	}
	t.offsets = append(t.offsets, offset)
	t.SectionReader = io.NewSectionReader(t, 0, offset+2*tarBlockSize)
	return t, nil
}

func (t *Tar) ModTime() time.Time { return time.Unix(t.view.CreatedUnix(), 0) }

func (t *Tar) ReadAt(buffer []byte, off int64) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	read := 0
	for read < len(buffer) {
		pos := off + int64(read)
		if pos >= t.Size() {
			return read, io.EOF
		}
		// The entry holding pos; len(paths) is the trailer.
		index := sort.Search(len(t.offsets), func(i int) bool { return t.offsets[i] > pos }) - 1
		var n int
		var err error
		if index == len(t.paths) {
			n = zeros(buffer[read:], t.Size()-pos)
		} else {
			n, err = t.readEntry(index, pos-t.offsets[index], buffer[read:])
		}
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

func (t *Tar) readEntry(index int, offset int64, buffer []byte) (int, error) {
	size := t.view.manifest.Entries[t.paths[index]].Size // 0 for the root and directories
	headerLen := t.offsets[index+1] - t.offsets[index] - size - tarPadding(size)
	switch {
	case offset < headerLen:
		var encoded bytes.Buffer
		if err := tar.NewWriter(&encoded).WriteHeader(t.header(t.paths[index])); err != nil {
			return 0, err
		}
		return copy(buffer, encoded.Bytes()[offset:]), nil
	case offset < headerLen+size:
		offset -= headerLen
		return t.readContent(index, offset, buffer[:min(int64(len(buffer)), size-offset)])
	default:
		return zeros(buffer, t.offsets[index+1]-t.offsets[index]-offset), nil
	}
}

func (t *Tar) readContent(index int, offset int64, buffer []byte) (int, error) {
	entry := t.view.manifest.Entries[t.paths[index]]
	if t.file == nil || t.file.index != index {
		t.closeFile()
		file, err := t.view.store.openObject(t.view.Source(), entry.ObjectRef, entry.Size)
		if err != nil {
			return 0, fmt.Errorf("pack %q: %w", t.paths[index], err)
		}
		t.file = &tarFile{index: index, file: file, check: newContentCheck(entry.ObjectRef, entry.Size)}
	}
	f := t.file
	var read int
	var err error
	// A resumed download starts mid-file: hash the part it skips first, so the
	// file is still verified by the time its last byte goes out.
	if f.check.fed < offset {
		_, err = io.Copy(checkWriter{f.check}, io.NewSectionReader(f.file, f.check.fed, offset-f.check.fed))
	}
	if err == nil {
		read, err = f.file.ReadAt(buffer, offset)
	}
	if err == nil && f.check.fed == offset {
		err = f.check.feed(buffer[:read])
	}
	if errors.Is(err, ErrIntegrity) {
		err = errors.Join(err, setAside(f.file.Name()))
	}
	if err != nil {
		return read, fmt.Errorf("pack %q: %w", t.paths[index], err)
	}
	return read, nil
}

// Safe to call repeatedly and during a read. The download's final Close follows
// its last Read, so nothing stays open.
func (t *Tar) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeFile()
}

func (t *Tar) closeFile() {
	if t.file != nil {
		_ = t.file.file.Close()
		t.file = nil
	}
}

// "" is the root folder itself.
func (t *Tar) header(logicalPath string) *tar.Header {
	header := &tar.Header{Typeflag: tar.TypeDir, Name: t.root + "/", Mode: 0o755, ModTime: t.ModTime()}
	if logicalPath == "" {
		return header
	}
	entry := t.view.manifest.Entries[logicalPath]
	header.Name += logicalPath
	if entry.Kind == entryDirectory {
		header.Name += "/"
		return header
	}
	header.Typeflag, header.Mode, header.Size = tar.TypeReg, 0o644, entry.Size
	return header
}

func zeros(buffer []byte, remaining int64) int {
	n := int(min(int64(len(buffer)), remaining))
	clear(buffer[:n])
	return n
}

func tarPadding(size int64) int64 {
	return (tarBlockSize - size%tarBlockSize) % tarBlockSize
}
