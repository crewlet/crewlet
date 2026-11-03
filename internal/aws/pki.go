package aws

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// The company's certificate authority: a P-256 key the engine generates,
// seals and never shows, whose certificate the owner registers as the Roles
// Anywhere trust anchor. Every CreateSession signs with a fresh in-memory leaf
// key under a short-lived leaf certificate, so the CA key is the one secret.

const (
	// CALifetime outlasts any plausible rotation cadence; a trust anchor
	// holds two certificates, so a new CA is added beside the old one first.
	CALifetime = 5 * 365 * 24 * time.Hour
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

// CACommonName names a company's CA; the suffix tells two of its CAs apart
// during a rotation.
func CACommonName(company Company) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "crewlet-" + string(company) + "-ca-" + hex.EncodeToString(b)
}

// GenerateCA mints a CA meeting Roles Anywhere's trust-anchor rules
// (X.509v3, CA:true, Certificate Sign, SHA-256) and returns the bundle to
// seal: the key's PEM, then the certificate's.
func GenerateCA(cn string, now time.Time) (string, error) {
	if !cnPattern.MatchString(cn) {
		return "", fmt.Errorf("aws: CA common name %q must match %s", cn, cnPattern)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := randomSerial()
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Crewlet"}},
		NotBefore:             now.Add(-notBeforeSkew),
		NotAfter:              now.Add(CALifetime),
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

// ParseCA reads a sealed bundle back, refusing one whose key is not its
// certificate's.
func ParseCA(bundle string) (*CA, error) {
	var ca CA
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch block.Type {
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("aws: the CA key: %w", err)
			}
			ca.key = k
		case "CERTIFICATE":
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("aws: the CA certificate: %w", err)
			}
			ca.Cert = c
		}
	}
	if ca.key == nil || ca.Cert == nil {
		return nil, errors.New("aws: the CA bundle needs its key and its certificate")
	}
	if pub, ok := ca.Cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&ca.key.PublicKey) {
		return nil, errors.New("aws: the CA certificate is not the CA key's")
	}
	return &ca, nil
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
