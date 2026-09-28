package ios

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"

	"howett.net/plist"
)

// PairRecord is the per-device plist pairing leaves on the host, in the form
// usbmuxd, netmuxd and Finder store.
type PairRecord struct {
	DeviceCertificate []byte `plist:"DeviceCertificate"` // PEM
	HostPrivateKey    []byte `plist:"HostPrivateKey"`    // PEM
	HostCertificate   []byte `plist:"HostCertificate"`   // PEM
	RootPrivateKey    []byte `plist:"RootPrivateKey"`    // PEM
	RootCertificate   []byte `plist:"RootCertificate"`   // PEM
	SystemBUID        string `plist:"SystemBUID"`
	HostID            string `plist:"HostID"`
	WiFiMACAddress    string `plist:"WiFiMACAddress"`
	EscrowBag         []byte `plist:"EscrowBag,omitempty"`
	UDID              string `plist:"UDID,omitempty"`
}

// ParsePairRecord accepts certificates and keys as PEM, bare base64 or DER
// and returns them as PEM; only what a session needs is required.
func ParsePairRecord(data []byte) (*PairRecord, error) {
	var record PairRecord
	if _, err := plist.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("decode pairing record: %w", err)
	}
	for _, field := range []struct {
		value     *[]byte
		blockType string
	}{
		{&record.DeviceCertificate, "CERTIFICATE"},
		{&record.HostCertificate, "CERTIFICATE"},
		{&record.RootCertificate, "CERTIFICATE"},
		{&record.HostPrivateKey, "PRIVATE KEY"},
		{&record.RootPrivateKey, "PRIVATE KEY"},
	} {
		*field.value = asPEM(*field.value, field.blockType)
	}
	switch {
	case record.HostID == "" || record.SystemBUID == "":
		return nil, errors.New("pairing record without HostID or SystemBUID")
	case len(record.HostCertificate) == 0 || len(record.HostPrivateKey) == 0:
		return nil, errors.New("pairing record without a host certificate and key")
	}
	return &record, nil
}

func (r *PairRecord) Marshal() ([]byte, error) {
	return plist.MarshalIndent(r, plist.XMLFormat, "\t")
}

// TLSConfig authenticates this host. The device's certificate is issued at
// pairing, not publicly trusted, so like every lockdown client it is unverified.
func (r *PairRecord) TLSConfig() (*tls.Config, error) {
	cert, err := tls.X509KeyPair(r.HostCertificate, r.HostPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("pairing record host identity: %w", err)
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		ServerName:         "Device",
		InsecureSkipVerify: true, //nolint:gosec // see above
	}, nil
}

func asPEM(data []byte, blockType string) []byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("-----BEGIN ")) {
		return data
	}
	der := trimmed
	if decoded, err := base64.StdEncoding.DecodeString(string(bytes.Join(bytes.Fields(trimmed), nil))); err == nil {
		der = decoded
	}
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}
