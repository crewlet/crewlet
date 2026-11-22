// Package es256 signs JSON Web Tokens with a P-256 key (RFC 7518's ES256), for
// the vendors that take a token the engine signs itself: App Store Connect
// signs every request with a team key, and Apple Ads a client secret.
//
// A leaf of the standard library. The private key is read from one PEM block,
// PKCS #8 (App Store Connect's .p8 file) or SEC 1 (the `EC PRIVATE KEY` OpenSSL
// writes, which Apple Ads asks for), held only in memory by its caller, and
// never written anywhere.
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
	"regexp"
	"strings"
	"sync"
)

// ErrNotP256 is a value that is not one PEM block holding a P-256 key.
var ErrNotP256 = errors.New("es256: not one PKCS #8 or SEC 1 PEM block holding a P-256 private key")

// ParseKey reads one PEM private key holding a P-256 key: PKCS #8 (`BEGIN
// PRIVATE KEY`), as App Store Connect's .p8 file and the engine's minted keys
// are, or SEC 1 (`BEGIN EC PRIVATE KEY`), as `openssl ecparam -genkey` writes
// Apple Ads' key. Text around the block is refused, so a paste of two keys is
// not one.
func ParseKey(value string) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode([]byte(NormalisePEM(value)))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return nil, ErrNotP256
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrNotP256
		}
		key, _ = parsed.(*ecdsa.PrivateKey)
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrNotP256
		}
		key = parsed
	}
	if key == nil || key.Curve != elliptic.P256() {
		return nil, ErrNotP256
	}
	return key, nil
}

// pemArmour is one PEM block, its line breaks any or none: its BEGIN type, its
// body, and its END type. Compiled at first use, not at init (setup's shape.go
// says why).
var pemArmour = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]+)-----([A-Za-z0-9+/=\s]*)-----END ([A-Z0-9 ]+)-----`)
})

// NormalisePEM is value's PEM blocks written as [pem.Encode] writes them, each
// body re-wrapped at 64 characters, whatever line breaks the value arrived with,
// and trimmed as every stored value is.
//
// A ONE-LINE BOX STRIPS LINE BREAKS: a .p8 file pasted into a password input
// reaches the engine as `-----BEGIN PRIVATE KEY-----MIGT...-----END PRIVATE
// KEY-----`, which [pem.Decode] refuses, and some tools flatten them to spaces.
// Base64 holds no whitespace, so dropping it from the body loses nothing.
// Anything else is returned as it came, for the parser to refuse: text around
// or between the blocks, a body holding headers (an encrypted key), or a BEGIN
// and END that disagree.
func NormalisePEM(value string) string {
	value = strings.TrimSpace(value)
	found := pemArmour().FindAllStringSubmatchIndex(value, -1)
	if len(found) == 0 {
		return value
	}
	blocks := make([]string, 0, len(found))
	last := 0
	for _, m := range found {
		if strings.TrimSpace(value[last:m[0]]) != "" {
			return value
		}
		kind, body, end := value[m[2]:m[3]], value[m[4]:m[5]], value[m[6]:m[7]]
		if kind != end {
			return value
		}
		body = strings.Join(strings.Fields(body), "")
		lines := []string{"-----BEGIN " + kind + "-----"}
		for len(body) > 64 {
			lines = append(lines, body[:64])
			body = body[64:]
		}
		if body != "" {
			lines = append(lines, body)
		}
		blocks = append(blocks, strings.Join(append(lines, "-----END "+kind+"-----"), "\n"))
		last = m[1]
	}
	if strings.TrimSpace(value[last:]) != "" {
		return value
	}
	return strings.Join(blocks, "\n")
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
