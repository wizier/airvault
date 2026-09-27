package objectstore

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"
)

const tarBlockSize = 512

// Tar is a snapshot packed as an uncompressed tar archive under one root
// folder, streamed straight from the objects: nothing is staged on disk, and
// its exact length is known before the first byte is written.
type Tar struct {
	view  *View
	root  string
	paths []string // "" (the root folder) first, then sorted: parents precede children
	size  int64
}

// Tar prepares the archive of this snapshot with every entry under root.
func (v *View) Tar(root string) (*Tar, error) {
	t := &Tar{view: v, root: root,
		paths: append([]string{""}, slices.Sorted(maps.Keys(v.manifest.Entries))...)}
	// tar.Writer.Close ends an archive with two zero blocks.
	t.size = 2 * tarBlockSize
	for _, logicalPath := range t.paths {
		header := t.header(logicalPath)
		var encoded bytes.Buffer
		if err := tar.NewWriter(&encoded).WriteHeader(header); err != nil {
			return nil, err
		}
		t.size += int64(encoded.Len()) + header.Size + (tarBlockSize-header.Size%tarBlockSize)%tarBlockSize
	}
	return t, nil
}

// Size is the exact length WriteTo produces.
func (t *Tar) Size() int64 { return t.size }

func (t *Tar) WriteTo(w io.Writer) (int64, error) {
	counter := &countingWriter{w: w}
	archive := tar.NewWriter(counter)
	for _, logicalPath := range t.paths {
		header := t.header(logicalPath)
		if err := archive.WriteHeader(header); err != nil {
			return counter.n, err
		}
		if header.Typeflag == tar.TypeReg {
			if err := t.copyFile(archive, logicalPath); err != nil {
				return counter.n, fmt.Errorf("pack %q: %w", logicalPath, err)
			}
		}
	}
	err := archive.Close()
	return counter.n, err
}

// header describes one entry; "" is the root folder itself.
func (t *Tar) header(logicalPath string) *tar.Header {
	header := &tar.Header{Typeflag: tar.TypeDir, Name: t.root + "/", Mode: 0o755,
		ModTime: time.Unix(t.view.CreatedUnix(), 0)}
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

// copyFile checks the copied bytes against the object hash, so a damaged
// object fails the stream instead of passing as a complete copy.
func (t *Tar) copyFile(w io.Writer, logicalPath string) error {
	file, err := t.view.Open(logicalPath)
	if err != nil {
		return err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(w, io.TeeReader(file, digest)); err != nil {
		return err
	}
	if want := t.view.manifest.Entries[logicalPath].ObjectRef; hex.EncodeToString(digest.Sum(nil)) != want {
		return fmt.Errorf("object %q content does not match its hash", want)
	}
	return nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(data []byte) (int, error) {
	written, err := c.w.Write(data)
	c.n += int64(written)
	return written, err
}
