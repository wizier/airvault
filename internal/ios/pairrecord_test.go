package ios_test

import (
	"bytes"
	"encoding/base64"
	"encoding/pem"
	"reflect"
	"testing"

	"howett.net/plist"

	"github.com/wizier/airvault/internal/ios"
	"github.com/wizier/airvault/internal/ios/iostest"
)

func TestPairRecordRoundTrip(t *testing.T) {
	record, _ := iostest.NewPairing(t)
	for _, format := range []int{plist.XMLFormat, plist.BinaryFormat} {
		data, err := plist.Marshal(record, format)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ios.ParsePairRecord(data)
		if err != nil {
			t.Fatalf("format %d: %v", format, err)
		}
		if !reflect.DeepEqual(parsed, record) {
			t.Fatalf("format %d: round trip changed the record", format)
		}
		if _, err := parsed.TLSConfig(); err != nil {
			t.Fatal(err)
		}
	}
	xml, err := record.Marshal()
	if err != nil || !bytes.HasPrefix(xml, []byte("<?xml")) {
		t.Fatalf("Marshal must write XML: %.40q, %v", xml, err)
	}
}

// Other tools store certificates as bare base64 or DER; they read as PEM.
func TestPairRecordAcceptsBase64AndDER(t *testing.T) {
	original, _ := iostest.NewPairing(t)
	record := *original
	hostDER, _ := pem.Decode(record.HostCertificate)
	keyDER, _ := pem.Decode(record.HostPrivateKey)
	record.HostCertificate = []byte(base64.StdEncoding.EncodeToString(hostDER.Bytes))
	record.HostPrivateKey = keyDER.Bytes
	data, err := plist.Marshal(&record, plist.XMLFormat)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ios.ParsePairRecord(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.TLSConfig(); err != nil {
		t.Fatalf("normalized identity is unusable: %v", err)
	}
}

func TestPairRecordRequiresIdentity(t *testing.T) {
	original, _ := iostest.NewPairing(t)
	record := *original
	record.HostID = ""
	data, _ := plist.Marshal(&record, plist.XMLFormat)
	if _, err := ios.ParsePairRecord(data); err == nil {
		t.Fatal("a record without HostID parsed")
	}
	if _, err := ios.ParsePairRecord([]byte("not a plist")); err == nil {
		t.Fatal("garbage parsed")
	}
}
