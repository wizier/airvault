package objectstore

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/wizier/airvault/internal/domain"
)

const (
	formatVersion   = 1
	maxManifestSize = 512 << 20
	maxEntries      = 2_000_000
	maxKeyLength    = 4096
	maxKeyComponent = 255
	maxKeyDepth     = 128
)

const (
	entryFile      = "file"
	entryDirectory = "directory"
)

// ErrManifestCorrupt marks a manifest read in full whose content is not a
// valid sealed manifest — a judgment about content, never about I/O. Startup
// reconciliation deletes these so they cannot wedge object collection.
var ErrManifestCorrupt = errors.New("corrupt manifest")

func corrupt(err error) error {
	return fmt.Errorf("%w: %w", ErrManifestCorrupt, err)
}

type manifestEntry struct {
	Kind         string `json:"kind"`
	ObjectRef    string `json:"objectRef,omitempty"`
	Size         int64  `json:"size,omitempty"`
	ModifiedUnix int64  `json:"modifiedUnix,omitempty"`
}

// The portable wire contract of a snapshot: changing it breaks existing stores.
type manifestProjection struct {
	Version     int    `json:"version"`
	SourceUDID  string `json:"sourceUdid"`
	SnapshotID  string `json:"snapshotId"`
	CreatedUnix int64  `json:"createdUnix"`
	// SizeBytes is the sum of every file entry's size, duplicates included —
	// the backup size, recomputed from entries at load.
	SizeBytes     int64                    `json:"sizeBytes"`
	EntriesSHA256 string                   `json:"entriesSha256"`
	Entries       map[string]manifestEntry `json:"entries"`
}

type View struct {
	store    *Store
	relative string
	manifest *manifestProjection
}

// A sealed but unpublished snapshot. Only Publish accepts it, so the type rules
// out publishing a manifest twice.
type StagingView struct{ View }

func (v *View) Source() string { return v.manifest.SourceUDID }

func (v *View) ID() string { return v.manifest.SnapshotID }

func (v *View) SizeBytes() int64 { return v.manifest.SizeBytes }

// CreatedUnix is when the immutable snapshot was created: the moment its
// contents were finalized. It rebuilds created_at after SQLite is lost.
func (v *View) CreatedUnix() int64 { return v.manifest.CreatedUnix }

// FileSize returns the size of a regular file in the snapshot.
func (v *View) FileSize(key string) (int64, bool) {
	entry, ok := v.manifest.Entries[key]
	if !ok || entry.Kind != entryFile {
		return 0, false
	}
	return entry.Size, true
}

// Open verifies content against its address as it is read.
func (v *View) Open(key string) (io.ReadCloser, error) {
	entry, ok := v.manifest.Entries[key]
	if !ok || entry.Kind != entryFile {
		return nil, fs.ErrNotExist
	}
	return v.store.openReader(v.Source(), key, entry.ObjectRef, entry.Size, nil)
}

func (s *Store) OpenSnapshot(source, snapshotID string) (*View, error) {
	return s.openManifest(source, snapshotID, snapshotManifestRelative(source, snapshotID))
}

func (s *Store) openManifest(source, snapshotID, relative string) (*View, error) {
	if err := validateSnapshotIdentity(source, snapshotID); err != nil {
		return nil, err
	}
	manifest, err := s.loadManifest(relative)
	if err != nil {
		return nil, err
	}
	if manifest.SourceUDID != source || manifest.SnapshotID != snapshotID {
		return nil, corrupt(fmt.Errorf("manifest %q identity mismatch", relative))
	}
	return &View{store: s, relative: relative, manifest: manifest}, nil
}

func (s *Store) loadManifest(relative string) (*manifestProjection, error) {
	manifestPath, err := s.resolveManifest(relative)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxManifestSize {
		return nil, corrupt(fmt.Errorf("manifest %q has invalid size", relative))
	}
	// Unknown fields are ignored: rejecting them would break every read after an
	// additive field lands, before formatVersion, the gate meant for that, runs.
	manifest := new(manifestProjection)
	decoder := json.NewDecoder(io.LimitReader(file, maxManifestSize+1))
	if err := decoder.Decode(manifest); err != nil {
		// The stat above fixed the size, so hitting EOF mid-value is truncated
		// content, not a read failure; syntax and type errors likewise judge
		// bytes that were delivered. Anything else may be transient I/O.
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		wrapped := fmt.Errorf("decode object manifest %q: %w", relative, err)
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, corrupt(wrapped)
		}
		return nil, wrapped
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, corrupt(fmt.Errorf("decode object manifest %q: trailing JSON value", relative))
		}
		return nil, fmt.Errorf("decode object manifest %q: trailing data: %w", relative, err)
	}
	// A newer format is a newer writer's, not damage: it is kept, and collection
	// stops for its source instead of sweeping what it references.
	if manifest.Version > formatVersion {
		return nil, fmt.Errorf("manifest %q uses newer format %d", relative, manifest.Version)
	}
	if err := validateManifestHeader(relative, manifest); err != nil {
		return nil, corrupt(err)
	}
	facts, err := inspectManifestEntries(relative, manifest)
	if err != nil {
		return nil, corrupt(err)
	}
	if facts.sizeBytes != manifest.SizeBytes {
		return nil, corrupt(fmt.Errorf(
			"manifest %q size is %d, entries total %d",
			relative, manifest.SizeBytes, facts.sizeBytes,
		))
	}
	if facts.entriesSHA256 != manifest.EntriesSHA256 {
		return nil, corrupt(fmt.Errorf("manifest %q entries checksum mismatch", relative))
	}
	return manifest, nil
}

func validateManifestHeader(relative string, manifest *manifestProjection) error {
	if manifest.Version != formatVersion {
		return fmt.Errorf("manifest %q uses an unsupported format", relative)
	}
	if err := domain.ValidateSource(manifest.SourceUDID); err != nil {
		return fmt.Errorf("manifest %q: %w", relative, err)
	}
	if err := validateSnapshotID(manifest.SnapshotID); err != nil {
		return fmt.Errorf("manifest %q: %w", relative, err)
	}
	if len(manifest.Entries) > maxEntries {
		return fmt.Errorf("manifest %q exceeds %d entries", relative, maxEntries)
	}
	if manifest.Entries == nil || manifest.SizeBytes < 0 || manifest.CreatedUnix <= 0 || !validSHA256(manifest.EntriesSHA256) {
		return fmt.Errorf("manifest %q has invalid aggregate metadata", relative)
	}
	return nil
}

type manifestEntryFacts struct {
	sizeBytes     int64
	entriesSHA256 string
}

func inspectManifestEntries(relative string, manifest *manifestProjection) (manifestEntryFacts, error) {
	seal := newManifestEntriesSeal()
	var snapshotSize int64
	for _, logicalPath := range slices.Sorted(maps.Keys(manifest.Entries)) {
		entry := manifest.Entries[logicalPath]
		if !ValidKey(logicalPath) {
			return manifestEntryFacts{}, fmt.Errorf("manifest %q has invalid key %q", relative, logicalPath)
		}
		if parent := path.Dir(logicalPath); parent != "." {
			parentEntry, ok := manifest.Entries[parent]
			if !ok {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q path %q is missing parent directory %q", relative, logicalPath, parent)
			}
			if parentEntry.Kind != entryDirectory {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q parent %q of %q is not a directory", relative, parent, logicalPath)
			}
		}
		switch entry.Kind {
		case entryDirectory:
			if entry.ObjectRef != "" || entry.Size != 0 {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q directory %q has file content", relative, logicalPath)
			}
		case entryFile:
			if entry.Size < 0 {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q file %q has negative size", relative, logicalPath)
			}
			if err := validateObjectRef(entry.ObjectRef); err != nil {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q file %q: %w", relative, logicalPath, err)
			}
			if entry.Size > math.MaxInt64-snapshotSize {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q snapshot size overflows int64", relative)
			}
			snapshotSize += entry.Size
		default:
			return manifestEntryFacts{}, fmt.Errorf("manifest %q path %q has invalid kind %q", relative, logicalPath, entry.Kind)
		}
		seal.add(logicalPath, entry)
	}
	return manifestEntryFacts{
		sizeBytes:     snapshotSize,
		entriesSHA256: seal.checksum(),
	}, nil
}

func validSHA256(value string) bool {
	return validLowerHex(value, sha256.Size*2)
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

// manifestEntriesSeal is SHA-256 over a length-prefixed, little-endian encoding
// of the entries in sorted-key order. Changing the layout makes every existing
// store unreadable.
type manifestEntriesSeal struct {
	digest  hash.Hash
	scratch [8]byte
}

func newManifestEntriesSeal() *manifestEntriesSeal {
	return &manifestEntriesSeal{digest: sha256.New()}
}

func (seal *manifestEntriesSeal) add(key string, entry manifestEntry) {
	seal.field([]byte(key))
	seal.field([]byte(entry.Kind))
	seal.field([]byte(entry.ObjectRef))
	seal.integer(entry.Size)
	seal.integer(entry.ModifiedUnix)
}

func (seal *manifestEntriesSeal) field(value []byte) {
	binary.LittleEndian.PutUint64(seal.scratch[:], uint64(len(value)))
	_, _ = seal.digest.Write(seal.scratch[:])
	_, _ = seal.digest.Write(value)
}

func (seal *manifestEntriesSeal) integer(value int64) {
	binary.LittleEndian.PutUint64(seal.scratch[:], uint64(value))
	_, _ = seal.digest.Write(seal.scratch[:])
}

func (seal *manifestEntriesSeal) checksum() string {
	return fmt.Sprintf("%x", seal.digest.Sum(nil))
}

// ValidKey reports a key the store accepts: a clean relative path of at most
// 4096 bytes and 128 components of at most 255 bytes, free of characters that
// tools mishandle.
func ValidKey(key string) bool {
	if !fs.ValidPath(key) || key == "." || len(key) > maxKeyLength || strings.ContainsAny(key, "\\\x00\u2028\u2029") {
		return false
	}
	depth := 0
	for part := range strings.SplitSeq(key, "/") {
		if depth++; depth > maxKeyDepth || len(part) > maxKeyComponent {
			return false
		}
	}
	return true
}
