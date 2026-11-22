// Package es256 signs JSON Web Tokens with a P-256 key (RFC 7518's ES256), for
// the vendors that take a token the engine signs itself: App Store Connect
// signs every request with a team key, and Apple Ads a client secret.
//
// A leaf of the standard library. The private key is read from a PKCS#8 PEM
// block, held only in memory by its caller, and never written anywhere.
package es256

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// ErrNotP256 is a value that is not one PKCS#8 PEM block holding a P-256 key.
var ErrNotP256 = errors.New("es256: not one PKCS#8 PEM block holding a P-256 private key")

// ParseKey reads a PKCS#8 PEM private key (`BEGIN PRIVATE KEY`) holding a
// P-256 key, as App Store Connect's .p8 file and the engine's minted keys are.
// Text around the block is refused, so a paste of two keys is not one.
func ParseKey(value string) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(value)))
	if block == nil || block.Type != "PRIVATE KEY" || strings.TrimSpace(string(rest)) != "" {
		return nil, ErrNotP256
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, ErrNotP256
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, ErrNotP256
	}
	return key, nil
}

// Sign is a compact JWS of claims under key, its header naming kid and typ.
// The signature is RFC 7518's fixed-width r || s, never ASN.1, which every
// verifier of these tokens refuses.
func Sign(key *ecdsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": kid, "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("es256: encode the header: %w", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("es256: encode the claims: %w", err)
	}
	input := b64(header) + "." + b64(body)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("es256: sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
