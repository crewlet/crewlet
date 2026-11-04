package setup

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A minted certificate authority: a P-256 key the engine generates, seals and
// never shows, whose certificate a vendor registers as its trust anchor (AWS
// IAM Roles Anywhere). The engine signs a short-lived client certificate with
// it for each sign-in, so the key is the one secret.
const (
	// CertificateAuthorityLifetime outlasts any plausible rotation cadence;
	// a trust anchor holds two certificates, so a new one is added beside
	// the old one first.
	CertificateAuthorityLifetime = 5 * 365 * 24 * time.Hour
	// notBeforeSkew tolerates a host clock behind the vendor's.
	notBeforeSkew = 5 * time.Minute
)

// caName keeps a certificate authority's name inside what a vendor's tag
// and condition patterns accept.
var caName = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^crewlet-[a-z0-9-]{1,53}$`)
})

// CertificateAuthorityName names a new certificate authority; the suffix
// tells two of them apart while a trust anchor holds both.
func CertificateAuthorityName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return "crewlet-ca-" + hex.EncodeToString(b)
}

// GenerateCertificateAuthority mints a certificate authority meeting AWS's
// trust anchor rules (X.509v3, CA:true, Certificate Sign, SHA-256) and returns
// the bundle to seal: the key's PEM, then the certificate's.
func GenerateCertificateAuthority(name string, now time.Time) (string, error) {
	if !caName().MatchString(name) {
		return "", fmt.Errorf("setup: certificate authority name %q must match %s", name, caName())
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name, Organization: []string{"Crewlet"}},
		NotBefore:             now.Add(-notBeforeSkew),
		NotAfter:              now.Add(CertificateAuthorityLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})) +
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}

// ParseCertificateAuthority reads a minted bundle back: its certificate, a
// certificate authority's, and its key, which must be the certificate's.
func ParseCertificateAuthority(bundle string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	var cert *x509.Certificate
	var key *ecdsa.PrivateKey
	rest := []byte(bundle)
	for {
		var block *pem.Block
		if block, rest = pem.Decode(rest); block == nil {
			break
		}
		var err error
		switch block.Type {
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		case "CERTIFICATE":
			cert, err = x509.ParseCertificate(block.Bytes)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("setup: the certificate authority's %s: %w", strings.ToLower(block.Type), err)
		}
	}
	switch pub, ok := certKey(cert); {
	case key == nil || cert == nil:
		return nil, nil, errors.New("setup: a certificate authority bundle holds its key and its certificate")
	case !cert.IsCA:
		return nil, nil, errors.New("setup: the certificate is not a certificate authority's")
	case !ok || !pub.Equal(&key.PublicKey):
		return nil, nil, errors.New("setup: the certificate is not the key's")
	}
	return cert, key, nil
}

func certKey(cert *x509.Certificate) (*ecdsa.PublicKey, bool) {
	if cert == nil {
		return nil, false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return pub, ok
}
