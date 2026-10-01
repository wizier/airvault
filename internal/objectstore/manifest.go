package objectstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/wizier/airvault/internal/domain"
	"github.com/wizier/airvault/internal/durable"
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
// valid sealed manifest — a judgment about content, never about I/O. Its
// restore point is marked damaged, and the objects it still names are kept.
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
type manifest struct {
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

func (s *Store) loadManifest(relative string) (*manifest, error) {
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
	decoded := new(manifest)
	decoder := json.NewDecoder(io.LimitReader(file, maxManifestSize+1))
	if err := decoder.Decode(decoded); err != nil {
		// The stat above fixed the size, so hitting EOF mid-value is truncated
		// content, not a read failure; syntax and type errors likewise judge
		// bytes that were delivered. Anything else may be transient I/O.
		_, syntax := errors.AsType[*json.SyntaxError](err)
		_, typed := errors.AsType[*json.UnmarshalTypeError](err)
		wrapped := fmt.Errorf("decode object manifest %q: %w", relative, err)
		if syntax || typed || errors.Is(err, io.ErrUnexpectedEOF) {
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
	if decoded.Version > formatVersion {
		return nil, fmt.Errorf("manifest %q uses newer format %d", relative, decoded.Version)
	}
	if err := validateManifestHeader(relative, decoded); err != nil {
		return nil, corrupt(err)
	}
	facts, err := inspectManifestEntries(relative, decoded)
	if err != nil {
		return nil, corrupt(err)
	}
	if facts.sizeBytes != decoded.SizeBytes {
		return nil, corrupt(fmt.Errorf(
			"manifest %q size is %d, entries total %d",
			relative, decoded.SizeBytes, facts.sizeBytes,
		))
	}
	if facts.entriesSHA256 != decoded.EntriesSHA256 {
		return nil, corrupt(fmt.Errorf("manifest %q entries checksum mismatch", relative))
	}
	return decoded, nil
}

func writeManifest(path string, m *manifest) error {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(m); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if encoded.Len() > maxManifestSize {
		return fmt.Errorf("%w: manifest of %d bytes", ErrIntegrity, encoded.Len())
	}
	return durable.WriteFile(path, encoded.Bytes(), 0o644)
}

func validateManifestHeader(relative string, m *manifest) error {
	if m.Version != formatVersion {
		return fmt.Errorf("manifest %q uses an unsupported format", relative)
	}
	if err := domain.ValidateSource(m.SourceUDID); err != nil {
		return fmt.Errorf("manifest %q: %w", relative, err)
	}
	if err := validateSnapshotID(m.SnapshotID); err != nil {
		return fmt.Errorf("manifest %q: %w", relative, err)
	}
	if len(m.Entries) > maxEntries {
		return fmt.Errorf("manifest %q exceeds %d entries", relative, maxEntries)
	}
	if m.Entries == nil || m.SizeBytes < 0 || m.CreatedUnix <= 0 || !validSHA256(m.EntriesSHA256) {
		return fmt.Errorf("manifest %q has invalid aggregate metadata", relative)
	}
	return nil
}

type manifestEntryFacts struct {
	sizeBytes     int64
	entriesSHA256 string
}

func inspectManifestEntries(relative string, m *manifest) (manifestEntryFacts, error) {
	seal := newManifestEntriesSeal()
	var snapshotSize int64
	var err error
	for _, logicalPath := range slices.Sorted(maps.Keys(m.Entries)) {
		entry := m.Entries[logicalPath]
		if !ValidKey(logicalPath) {
			return manifestEntryFacts{}, fmt.Errorf("manifest %q has invalid key %q", relative, logicalPath)
		}
		if parent := path.Dir(logicalPath); parent != "." {
			parentEntry, ok := m.Entries[parent]
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
			if snapshotSize, err = addChecked(snapshotSize, entry.Size); err != nil {
				return manifestEntryFacts{}, fmt.Errorf("manifest %q: %w", relative, err)
			}
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
