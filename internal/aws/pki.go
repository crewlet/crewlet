package aws

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/setup"
)

// The company's certificate authority, minted and sealed by setup
// ([setup.ShapeCertificateAuthority]): every CreateSession signs with a fresh
// in-memory leaf key under a short-lived leaf certificate, so the CA key is the
// one secret.

const (
	// LeafLifetime only has to outlive one CreateSession call; a leaf that
	// leaks is useless a quarter of an hour later.
	LeafLifetime = 15 * time.Minute
	// notBeforeSkew tolerates a host clock behind AWS's.
	notBeforeSkew = 5 * time.Minute
)

// cnPattern keeps a CN inside what STS accepts as a source identity behind
// Roles Anywhere's "CN=" prefix (61 characters) and inside the tag pattern.
var cnPattern = regexp.MustCompile(`^crewlet-[a-z0-9-]{1,53}$`)

// LeafKind picks the leaf's key type, and so its signing algorithm.
type LeafKind int

const (
	LeafECDSA LeafKind = iota // AWS4-X509-ECDSA-SHA256, what the engine signs with
	LeafRSA                   // AWS4-X509-RSA-SHA256, which the live probe also proved
)

// CA is the company's certificate authority, parsed from its sealed bundle.
type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// ParseCA reads a sealed bundle back ([setup.ParseCertificateAuthority]).
func ParseCA(bundle string) (*CA, error) {
	cert, key, err := setup.ParseCertificateAuthority(bundle)
	if err != nil {
		return nil, fmt.Errorf("aws: %s", strings.TrimPrefix(err.Error(), "setup: "))
	}
	return &CA{Cert: cert, key: key}, nil
}

// CertificatePEM is the CA's public half: what the owner registers.
func (ca *CA) CertificatePEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw}))
}

// Leaf mints an end-entity certificate for one CreateSession: CA:false,
// Digital Signature, and the CN that becomes the session's source identity.
func (ca *CA) Leaf(cn string, now time.Time, kind LeafKind) (*x509.Certificate, crypto.Signer, error) {
	if !cnPattern.MatchString(cn) {
		return nil, nil, fmt.Errorf("aws: leaf common name %q must match %s", cn, cnPattern)
	}
	var key crypto.Signer
	var err error
	if kind == LeafRSA {
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-notBeforeSkew),
		NotAfter:              now.Add(LeafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

// hasPEMPrivateKey reports text that carries a private key, which nothing
// shown or rendered may.
func hasPEMPrivateKey(s string) bool { return strings.Contains(s, "PRIVATE KEY") }
