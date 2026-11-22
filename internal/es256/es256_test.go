package es256_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/es256"
)

func pemOf(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// A token verifies under the key's public half with a fixed-width signature,
// and names its key and algorithm; only one PKCS#8 P-256 block is a key.
//
// Mutation: sign ASN.1, or accept a P-384 or RSA key, and a vendor refuses
// every token, or a key of another curve is taken for one.
func TestATokenVerifiesUnderItsKeysPublicHalf(t *testing.T) {
	t.Parallel()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	key, err := es256.ParseKey(pemOf(t, priv))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := es256.Sign(key, "ABC123DEFG", map[string]any{"aud": "appstoreconnect-v1", "exp": 2})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if len(sig) != 64 || !ecdsa.Verify(&priv.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]),
		new(big.Int).SetBytes(sig[32:])) {
		t.Fatalf("the token does not verify: %s", tok)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header map[string]string
	if err := json.Unmarshal(raw, &header); err != nil || header["alg"] != "ES256" || header["kid"] != "ABC123DEFG" {
		t.Errorf("the header is %v (%v)", header, err)
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 1024)
	for name, bad := range map[string]string{"P-384": pemOf(t, p384), "RSA": pemOf(t, rsaKey),
		"two keys": pemOf(t, priv) + pemOf(t, priv), "not PEM": "MIG...", "empty": ""} {
		if _, err := es256.ParseKey(bad); err == nil {
			t.Errorf("%s read as a P-256 key", name)
		}
	}
}

func sec1Of(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

// An OpenSSL `EC PRIVATE KEY` (SEC 1), as Apple Ads asks a person to make,
// reads as the same P-256 key its PKCS #8 form does; SEC 1 of another curve,
// a SEC 1 body under the PKCS #8 label and two blocks are refused.
//
// Mutation: drop the SEC 1 case, and an Apple Ads key never reads; parse SEC 1
// without the curve check, and a P-384 key is taken for one.
func TestASEC1KeyReadsAsItsP256Key(t *testing.T) {
	t.Parallel()
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	key, err := es256.ParseKey(sec1Of(t, priv))
	if err != nil || !key.Equal(priv) {
		t.Fatalf("a SEC 1 P-256 key read as %v (%v)", key, err)
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(priv)
	mislabelled := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, bad := range map[string]string{"SEC 1 P-384": sec1Of(t, p384), "SEC 1 under the PKCS #8 label": mislabelled,
		"two SEC 1 keys": sec1Of(t, priv) + sec1Of(t, priv)} {
		if _, err := es256.ParseKey(bad); err == nil {
			t.Errorf("%s read as a P-256 key", name)
		}
	}
}
