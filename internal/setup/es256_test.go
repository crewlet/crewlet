package setup_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// signingKey is a mintable field whose value is an ES256 key.
var signingKey = setup.Requirement{
	Field: "signing_key", Label: "Signing key", Kind: setup.KindSecret, Mintable: true,
	ConfigPath: "integrations.datadog.signing_key", SecretName: "DATADOG_SIGNING_KEY",
	Shape: setup.ShapeES256Key,
}

// mintES256 is a key as Save mints one, failing the test where the mint asks
// for a token instead.
func mintES256(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	value, err := setup.ShapeES256Key.Mint(func() string {
		t.Fatal("an ES256 key was minted as a token")
		return ""
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("the mint is not a PKCS#8 PEM block: %q", block)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		t.Fatalf("the mint is not a P-256 key: %T, %v", parsed, err)
	}
	return value, key
}

// AN ES256 KEY IS MINTED ON SAVE AND SEALED WHOLE: a fresh P-256 key, PKCS#8 in
// PEM, taken by its field although it spans lines, under the field's own
// name. A value that is no such key is refused at the field, never quoted.
func TestAnES256KeyIsMintedOnSaveAndSealed(t *testing.T) {
	t.Parallel()
	value, _ := mintES256(t)
	if again, _ := mintES256(t); again == value {
		t.Fatal("two mints made one key")
	}
	rec := &recorder{}
	result, err := writer(rec).Write(t.Context(), []setup.Requirement{signingKey}, setup.Submission{
		Kind: integration.KindDatadog, Values: map[string]string{"signing_key": value},
		Summary: "connect", By: founder,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := rec.secrets["DATADOG_SIGNING_KEY"]; got != strings.TrimSpace(value) {
		t.Errorf("the store holds %d bytes under the key's name, want the minted key", len(got))
	}
	if len(result.Secrets) != 1 || result.Secrets[0] != "DATADOG_SIGNING_KEY" {
		t.Errorf("Write sealed %v, want the key's name", result.Secrets)
	}
	for _, bad := range []string{"a single line", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"} {
		refused := signingKey.Refusal(bad)
		if refused == nil || strings.Contains(refused.Problem, bad) {
			t.Errorf("a value that is no ES256 key was answered %v", refused)
		}
	}
}

// THE PUBLIC VIEW OF A KEY HOLDS ITS PUBLIC HALF AND NOTHING PRIVATE: a key set
// with one ES256 signing key, named by its thumbprint, whose point is the
// key's; no PEM, no `d` member, and the private scalar in none of the
// encodings it could be written in. A requirement whose view holds a private
// key is never sent.
func TestThePublicViewOfAKeyHoldsNothingPrivate(t *testing.T) {
	t.Parallel()
	value, key := mintES256(t)
	jwks, err := setup.ES256JWKS(value)
	if err != nil {
		t.Fatalf("ES256JWKS: %v", err)
	}
	kid, err := setup.ES256KeyID(value)
	if err != nil || kid == "" {
		t.Fatalf("ES256KeyID = %q, %v", kid, err)
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(jwks), &set); err != nil || len(set.Keys) != 1 {
		t.Fatalf("the key set %s is not one key: %v", jwks, err)
	}
	got := set.Keys[0]
	point, _ := key.PublicKey.Bytes()
	for member, want := range map[string]string{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": kid,
		"x": base64.RawURLEncoding.EncodeToString(point[1:33]),
		"y": base64.RawURLEncoding.EncodeToString(point[33:]),
	} {
		if got[member] != want {
			t.Errorf("%s = %q, want %q", member, got[member], want)
		}
	}
	if _, private := got["d"]; private {
		t.Fatal("the key set carries the private scalar as d")
	}
	scalar, err := key.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(value))
	for name, private := range map[string]string{
		"the PEM's body": base64.StdEncoding.EncodeToString(block.Bytes)[:40],
		"the PEM label":  "PRIVATE KEY",
		"d, base64url":   base64.RawURLEncoding.EncodeToString(scalar),
		"d, base64":      base64.StdEncoding.EncodeToString(scalar),
		"d, hex":         hex.EncodeToString(scalar),
	} {
		if strings.Contains(jwks, private) {
			t.Errorf("the public view holds %s", name)
		}
	}

	shown := signingKey
	shown.Public = &setup.PublicView{Text: "cat > jwks.json <<'EOF'\n" + jwks + "\nEOF", Language: "sh"}
	wire, err := json.Marshal(shown)
	if err != nil || !strings.Contains(string(wire), `"public":{"text":`) || !strings.Contains(string(wire), `"language":"sh"`) {
		t.Fatalf("the requirement's public view is sent as %s (%v)", wire, err)
	}
	shown.Public = &setup.PublicView{Text: value, Language: "sh"}
	if wire, err := json.Marshal(shown); err == nil {
		t.Errorf("a public view holding the private key was sent: %d bytes", len(wire))
	}
}

// A PUBLIC VIEW IS BUILT FROM PUBLIC MATERIAL ONLY, whatever the held value is:
// one holding the value it was rendered from, or a PEM private key of any
// kind, is refused, and one holding only public text is taken as it is. A
// requirement whose view holds the literal its credential field stores is
// never sent either.
func TestAPublicViewIsBuiltFromPublicMaterialOnly(t *testing.T) {
	t.Parallel()
	value, _ := mintES256(t)
	jwks, err := setup.ES256JWKS(value)
	if err != nil {
		t.Fatalf("ES256JWKS: %v", err)
	}
	view, err := setup.PublicOf(value, jwks, "json")
	if err != nil || view.Text != jwks || view.Language != "json" {
		t.Fatalf("PublicOf(the key set) = %+v, %v", view, err)
	}
	for name, text := range map[string]string{
		"the held value":         "echo '" + strings.TrimSpace(value) + "'",
		"a held token":           "export TOKEN=" + "held-token-value",
		"another kind of key":    "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n-----END EC PRIVATE KEY-----",
		"an encrypted key block": "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIF\n-----END ENCRYPTED PRIVATE KEY-----",
	} {
		held := value
		if name == "a held token" {
			held = "held-token-value"
		}
		if _, err := setup.PublicOf(held, text, "sh"); err == nil {
			t.Errorf("%s: a public view was built", name)
		}
	}
	literal := setup.Requirement{Field: "token", Kind: setup.KindSecret, Stored: "a-literal-token",
		Public: &setup.PublicView{Text: "curl -H 'Authorization: a-literal-token'", Language: "sh"}}
	if wire, err := json.Marshal(literal); err == nil {
		t.Errorf("a view holding the field's stored literal was sent: %d bytes", len(wire))
	}
	literal.Stored = "${TOKEN}"
	if _, err := json.Marshal(literal); err != nil {
		t.Errorf("a view beside a stored reference was refused: %v", err)
	}
}
