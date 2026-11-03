package googlecloud

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
	"time"

	"github.com/crewlet/crewlet/internal/org"
)

// The engine's federation token: what Google's provider is told to trust, and
// for how long one signed token is good. Ten minutes covers one exchange with
// room for a slow answer, well inside Google's six-hour recommendation.
const (
	issuerBase  = "https://crewlet.invalid/"
	subjectBase = "crewlet:"
	jwtLifetime = 10 * time.Minute
	// jwtSkew backdates iat so a clock a few seconds ahead of Google's is
	// not refused as a token from the future.
	jwtSkew = 10 * time.Second
)

// Issuer is the `iss` the engine signs for company with, which the provider is
// created with. A pure function of the company's name, so the connect form's
// Cloud Shell block, the provider and the engine always agree. It resolves
// nowhere: the provider holds the public key itself.
func Issuer(company string) string { return issuerBase + org.Slugify(company) }

// Subject is the `sub` the engine signs for company with, which the
// provider's attribute condition and the provisioner's Workload Identity User
// binding name.
func Subject(company string) string { return subjectBase + org.Slugify(company) }

// Audience is the provider's audience, its resource name under the IAM host:
// what STS is told the token is for.
func Audience(provider string) string { return "//iam.googleapis.com/" + strings.TrimSpace(provider) }

// Principal is the one federated identity the provisioner lets impersonate it:
// the company's subject in the provider's pool.
func Principal(provider, company string) string {
	pool, _, _ := strings.Cut(strings.TrimSpace(provider), "/providers/")
	return "principal://iam.googleapis.com/" + pool + "/subject/" + Subject(company)
}

// ParseFederationKey reads the engine's federation key: a PKCS#8 PEM holding a
// P-256 private key. The error never quotes the value.
func ParseFederationKey(text string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(text)))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("googlecloud: the federation key is not a PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("googlecloud: the federation key's PEM holds no PKCS#8 private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("googlecloud: the federation key is not an ES256 (P-256) key")
	}
	return key, nil
}

// JWK is one public key as the provider's uploaded JWKS holds it.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// PublicJWK is the key's public half, with its RFC 7638 thumbprint as its kid,
// which every token the engine signs names. It carries nothing private.
func PublicJWK(pub *ecdsa.PublicKey) (JWK, error) {
	ec, err := pub.ECDH()
	if err != nil {
		return JWK{}, fmt.Errorf("googlecloud: the federation key's public half: %w", err)
	}
	raw := ec.Bytes() // 0x04 || X || Y
	if len(raw) != 65 {
		return JWK{}, errors.New("googlecloud: the federation key's public point is not P-256")
	}
	x, y := b64(raw[1:33]), b64(raw[33:])
	thumb := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`))
	return JWK{Kty: "EC", Crv: "P-256", X: x, Y: y, Kid: b64(thumb[:]), Alg: "ES256", Use: "sig"}, nil
}

// JWKS is the JSON key set holding the federation key's public half, which the
// provider is created or updated with (`--jwk-json-path`). Never the private
// half.
func JWKS(text string) (string, error) {
	key, err := ParseFederationKey(text)
	if err != nil {
		return "", err
	}
	jwk, err := PublicJWK(&key.PublicKey)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string][]JWK{"keys": {jwk}})
	if err != nil {
		return "", fmt.Errorf("googlecloud: encode the JWKS: %w", err)
	}
	return string(body), nil
}

// signJWT signs the federation token for one exchange at now.
func signJWT(key *ecdsa.PrivateKey, issuer, subject, audience string, now time.Time) (string, error) {
	jwk, err := PublicJWK(&key.PublicKey)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": jwk.Kid})
	if err != nil {
		return "", fmt.Errorf("googlecloud: encode the token header: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": issuer, "sub": subject, "aud": audience,
		"iat": now.Add(-jwtSkew).Unix(), "exp": now.Add(jwtLifetime).Unix(),
	})
	if err != nil {
		return "", fmt.Errorf("googlecloud: encode the token claims: %w", err)
	}
	input := b64(header) + "." + b64(claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("googlecloud: sign the federation token: %w", err)
	}
	// JWS FIXED-WIDTH r || s, never ASN.1: a DER signature is refused.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
