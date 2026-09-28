package ios

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// Pair asks the device to trust this host. While the Trust dialog is open it
// answers ErrPairingDialogResponsePending; asking again does not stack dialogs.
func (l *Lockdown) Pair(ctx context.Context, hostID, systemBUID, hostName string) (*PairRecord, error) {
	devicePublicKey, err := Value[[]byte](ctx, l, "", "DevicePublicKey")
	if err != nil {
		return nil, err
	}
	wifiMAC, err := Value[string](ctx, l, "", "WiFiAddress")
	if err != nil {
		return nil, err
	}
	record, err := newPairRecord(devicePublicKey, hostID, systemBUID, wifiMAC)
	if err != nil {
		return nil, err
	}
	// The device gets the certificates, never the private keys.
	offered := map[string]any{
		"DevicePublicKey":   devicePublicKey,
		"DeviceCertificate": record.DeviceCertificate,
		"HostCertificate":   record.HostCertificate,
		"RootCertificate":   record.RootCertificate,
		"HostID":            hostID,
		"SystemBUID":        systemBUID,
		"WiFiMACAddress":    wifiMAC,
	}
	var reply struct {
		EscrowBag []byte `plist:"EscrowBag"`
	}
	if err := l.request(ctx, map[string]any{
		"Request":         "Pair",
		"HostName":        hostName,
		"PairRecord":      offered,
		"ProtocolVersion": "2",
		"PairingOptions":  map[string]any{"ExtendedPairingErrors": true},
	}, &reply); err != nil {
		return nil, fmt.Errorf("pair: %w", err)
	}
	record.EscrowBag = reply.EscrowBag
	return record, nil
}

// Unpair asks the device to forget hostID; over Wi-Fi only inside a session.
func (l *Lockdown) Unpair(ctx context.Context, hostID string) error {
	request := map[string]any{"Request": "Unpair", "PairRecord": map[string]any{"HostID": hostID}}
	if err := l.request(ctx, request, &struct{}{}); err != nil {
		return fmt.Errorf("unpair: %w", err)
	}
	return nil
}

// newPairRecord issues a pairing's certificates: one RSA key is root and host,
// and it signs the device's certificate for devicePublicKey (PKCS#1 PEM).
func newPairRecord(devicePublicKey []byte, hostID, systemBUID, wifiMAC string) (*PairRecord, error) {
	block, _ := pem.Decode(devicePublicKey)
	if block == nil {
		return nil, fmt.Errorf("%w: DevicePublicKey is not PEM", ErrProtocol)
	}
	deviceKey, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: DevicePublicKey: %v", ErrProtocol, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := func(subject pkix.Name) *x509.Certificate {
		return &x509.Certificate{
			SerialNumber:          big.NewInt(1),
			Subject:               subject,
			NotBefore:             now,
			NotAfter:              now.AddDate(10, 0, 0),
			BasicConstraintsValid: true,
			IsCA:                  true,
			SignatureAlgorithm:    x509.SHA256WithRSA,
		}
	}
	root := template(pkix.Name{})
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("root certificate: %w", err)
	}
	deviceDER, err := x509.CreateCertificate(rand.Reader, template(pkix.Name{CommonName: "Device"}), root, deviceKey, key)
	if err != nil {
		return nil, fmt.Errorf("device certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return &PairRecord{
		DeviceCertificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: deviceDER}),
		HostCertificate:   rootPEM,
		HostPrivateKey:    keyPEM,
		RootCertificate:   rootPEM,
		RootPrivateKey:    keyPEM,
		SystemBUID:        systemBUID,
		HostID:            hostID,
		WiFiMACAddress:    wifiMAC,
	}, nil
}
