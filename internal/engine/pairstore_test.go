package engine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wizier/airvault/internal/ios"
)

func testRecord() *ios.PairRecord {
	return &ios.PairRecord{
		DeviceCertificate: []byte("-----BEGIN CERTIFICATE-----\nZGV2aWNl\n-----END CERTIFICATE-----\n"),
		HostCertificate:   []byte("-----BEGIN CERTIFICATE-----\naG9zdA==\n-----END CERTIFICATE-----\n"),
		HostPrivateKey:    []byte("-----BEGIN PRIVATE KEY-----\na2V5\n-----END PRIVATE KEY-----\n"),
		RootCertificate:   []byte("-----BEGIN CERTIFICATE-----\ncm9vdA==\n-----END CERTIFICATE-----\n"),
		RootPrivateKey:    []byte("-----BEGIN PRIVATE KEY-----\ncm9vdGtleQ==\n-----END PRIVATE KEY-----\n"),
		SystemBUID:        "BUID",
		HostID:            "HOST",
		EscrowBag:         []byte("escrow"),
	}
}

func TestPairStoreKeepsPrivateRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "lockdown")
	if err := os.Mkdir(root, 0o755); err != nil { // main creates it world-readable
		t.Fatal(err)
	}
	store := &pairStore{root: root}
	if record, err := store.Load("PHONE"); record != nil || err != nil {
		t.Fatalf("missing record = %v, %v; want nil, nil", record, err)
	}
	if err := store.Save("PHONE", testRecord()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("PHONE")
	if err != nil || !reflect.DeepEqual(loaded, testRecord()) {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, filepath.Join(root, "PHONE.plist"), 0o600)
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".PHONE.plist.tmp-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	for range 2 { // a missing record deletes too
		if err := store.Delete("PHONE"); err != nil {
			t.Fatal(err)
		}
		if record, err := store.Load("PHONE"); record != nil || err != nil {
			t.Fatalf("after Delete = %v, %v", record, err)
		}
	}
}

// A record that does not parse counts as absent; one that cannot be read
// safely is an error, never "not paired".
func TestPairStoreUnusableRecords(t *testing.T) {
	root := t.TempDir()
	store := &pairStore{root: root}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("GARBAGE.plist", "not a plist")
	if record, err := store.Load("GARBAGE"); record != nil || err != nil {
		t.Fatalf("unparsable record = %v, %v; want nil, nil", record, err)
	}

	write("REAL.plist", "x")
	if err := os.Symlink(filepath.Join(root, "REAL.plist"), filepath.Join(root, "LINK.plist")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("LINK"); err == nil {
		t.Fatal("a symlinked record was followed")
	}

	write("HUGE.plist", strings.Repeat("x", maxRecordSize+1))
	if _, err := store.Load("HUGE"); err == nil {
		t.Fatal("an oversized record was read")
	}

	for _, udid := range []string{"", "../escape", "a/b", strings.Repeat("a", 65)} {
		var engineErr *Error
		if _, err := store.Load(udid); !errors.As(err, &engineErr) || engineErr.Kind != ErrorInvalidArgument {
			t.Errorf("Load(%q) = %v, want ErrorInvalidArgument", udid, err)
		}
	}
}

// The first identity reserved for a pending pairing stays until deleted.
func TestPairStoreReservesOneIdentity(t *testing.T) {
	store := &pairStore{root: t.TempDir()}
	first, err := store.ReserveIdentity("PHONE", pairIdentity{HostID: "H1", SystemBUID: "B1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ReserveIdentity("PHONE", pairIdentity{HostID: "H2", SystemBUID: "B2"})
	if err != nil || second != first {
		t.Fatalf("second reservation = %+v, %v; want the first %+v", second, err, first)
	}
	assertMode(t, filepath.Join(store.root, pendingDir, "PHONE.plist"), 0o600)
	if err := store.DeleteIdentity("PHONE"); err != nil {
		t.Fatal(err)
	}
	if identity, err := store.Identity("PHONE"); identity != nil || err != nil {
		t.Fatalf("Identity after delete = %v, %v", identity, err)
	}

	if err := os.WriteFile(filepath.Join(store.root, pendingDir, "PHONE.plist"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if identity, err := store.Identity("PHONE"); identity != nil || err != nil {
		t.Fatalf("unparsable identity = %v, %v; want nil, nil", identity, err)
	}
	if replaced, err := store.ReserveIdentity("PHONE", second); err != nil || replaced != second {
		t.Fatalf("reserving over an unparsable identity = %+v, %v", replaced, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
