package setup

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

// An ES256 signing key the engine mints and holds, and the public half it
// shows ([ShapeES256Key]).
//
// THE PRIVATE HALF NEVER LEAVES THE SECRET STORE: it is sealed like every
// other minted credential and read back only to sign. What a person is shown,
// and what a vendor is told to trust, is the public half alone, as a JSON Web
// Key Set ([ES256JWKS]) whose key id is the key's own thumbprint
// ([ES256KeyID]), so a token the engine signs names the key it was signed
// with and a key set can hold an old key beside a new one.

// pemPrivateKey is the PEM type of a PKCS#8 private key.
const pemPrivateKey = "PRIVATE KEY"

// mintES256 is a fresh P-256 private key, PKCS#8 in PEM.
func mintES256() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate an ES256 key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("encode an ES256 key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: pemPrivateKey, Bytes: der})), nil
}

// errNotES256 is a value that is not one PKCS#8 PEM block holding a P-256 key.
var errNotES256 = errors.New("not an ES256 private key: one PKCS#8 PEM block holding a P-256 key")

// parseES256 reads value as [mintES256] writes it.
func parseES256(value string) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode([]byte(strings.TrimSpace(value)))
	if block == nil || block.Type != pemPrivateKey || len(strings.TrimSpace(string(rest))) > 0 {
		return nil, errNotES256
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errNotES256
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errNotES256
	}
	return key, nil
}

// es256JWK is the public members of an ES256 key as RFC 7517 names them, in
// the lexicographic order RFC 7638's thumbprint hashes them.
type es256JWK struct {
	Crv string `json:"crv"`
	Kty string `json:"kty"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// publicJWK is the public half of key, its coordinates padded to the curve's
// 32 bytes as RFC 7518 requires.
func publicJWK(key *ecdsa.PrivateKey) (es256JWK, error) {
	raw, err := key.PublicKey.Bytes()
	if err != nil {
		return es256JWK{}, fmt.Errorf("read the ES256 key's public half: %w", err)
	}
	// AN UNCOMPRESSED POINT: 0x04, then X and Y at 32 bytes each.
	if len(raw) != 65 || raw[0] != 4 {
		return es256JWK{}, errNotES256
	}
	enc := base64.RawURLEncoding
	return es256JWK{Crv: "P-256", Kty: "EC", X: enc.EncodeToString(raw[1:33]), Y: enc.EncodeToString(raw[33:])}, nil
}

// thumbprint is jwk's RFC 7638 thumbprint, base64url over SHA-256.
func (jwk es256JWK) thumbprint() (string, error) {
	canonical, err := json.Marshal(jwk)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// ES256KeyID is the key id of value, an ES256 key [ShapeES256Key] mints: its
// public half's RFC 7638 thumbprint, which a token signed with it names.
func ES256KeyID(value string) (string, error) {
	key, err := parseES256(value)
	if err != nil {
		return "", err
	}
	jwk, err := publicJWK(key)
	if err != nil {
		return "", err
	}
	return jwk.thumbprint()
}

// ES256JWKS is the JSON Web Key Set holding the public half of value, an ES256
// key [ShapeES256Key] mints: one signing key, named by [ES256KeyID]. NEVER THE
// PRIVATE HALF: the set is built from the public point alone.
func ES256JWKS(value string) (string, error) {
	key, err := parseES256(value)
	if err != nil {
		return "", err
	}
	jwk, err := publicJWK(key)
	if err != nil {
		return "", err
	}
	kid, err := jwk.thumbprint()
	if err != nil {
		return "", err
	}
	type member struct {
		es256JWK
		Alg string `json:"alg"`
		Use string `json:"use"`
		Kid string `json:"kid"`
	}
	set, err := json.Marshal(struct {
		Keys []member `json:"keys"`
	}{Keys: []member{{es256JWK: jwk, Alg: "ES256", Use: "sig", Kid: kid}}})
	if err != nil {
		return "", err
	}
	return string(set), nil
}
