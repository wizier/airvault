package iostest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/wizier/airvault/internal/ios"
)

// NewPairing returns the host's record and the device's TLS identity.
func NewPairing(t testing.TB) (*ios.PairRecord, tls.Certificate) {
	t.Helper()
	rootKey, root := newCert(t, "Root", nil, nil)
	hostKey, host := newCert(t, "Host", rootKey, root)
	deviceKey, device := newCert(t, "Device", rootKey, root)
	identity, err := tls.X509KeyPair(certPEM(device), keyPEM(t, deviceKey))
	if err != nil {
		t.Fatal(err)
	}
	return &ios.PairRecord{
		DeviceCertificate: certPEM(device),
		HostCertificate:   certPEM(host),
		HostPrivateKey:    keyPEM(t, hostKey),
		RootCertificate:   certPEM(root),
		RootPrivateKey:    keyPEM(t, rootKey),
		SystemBUID:        BUID,
		HostID:            "IOSTEST-HOST",
		EscrowBag:         []byte("escrow"),
	}, identity
}

func newCert(t testing.TB, name string, signerKey *ecdsa.PrivateKey, signer *x509.Certificate) (*ecdsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  signer == nil,
		BasicConstraintsValid: true,
	}
	if signer == nil {
		signer, signerKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return key, cert
}

func certPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func keyPEM(t testing.TB, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// DevicePublicKey returns an RSA key in lockdown's DevicePublicKey form.
func DevicePublicKey(t testing.TB) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})
}
