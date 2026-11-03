package setup

import (
	"fmt"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/whsec"
	"github.com/crewlet/crewlet/internal/words"
)

// Shape is what a value has to look like, minted or pasted.
//
// ONE FIELD NEEDS ONE, and that is why this exists rather than every mint
// being the same random token: GitLab's signing secret is `whsec_` and base64
// over exactly 32 bytes, and a value of any other shape cannot be the HMAC
// key for a delivery it will then refuse. Every other mintable field here is a
// shared token the third-party app compares verbatim, and takes any string.
type Shape string

const (
	// ShapeToken is a shared secret compared verbatim, which is the
	// default and every mintable field but one.
	ShapeToken Shape = ""

	// ShapeSigningKey is the Standard Webhooks signing secret: `whsec_`
	// followed by standard base64 over a 32-byte key, with the DECODED
	// bytes as the HMAC key. See [whsec].
	ShapeSigningKey Shape = "signing_key"

	// ShapeOpaqueToken is a vendor-issued token of letters, digits, hyphens
	// and underscores alone, such as a Cloudflare API token: a pasted value
	// holding any other character is not one the vendor issued.
	ShapeOpaqueToken Shape = "opaque_token"

	// ShapeES256Key is an ECDSA P-256 private key the engine signs ES256
	// tokens with, PKCS#8 in PEM, which spans several lines. A vendor is
	// shown only its public half ([ES256JWKS]). See es256.go.
	ShapeES256Key Shape = "es256_key"

	// ShapeCertificateAuthority is a certificate authority the engine signs
	// short-lived client certificates with: a P-256 key, then its
	// self-signed certificate, both PEM ([GenerateCertificateAuthority]).
	// Only ever minted, never pasted.
	ShapeCertificateAuthority Shape = "certificate_authority"
)

// Multiline reports a shape whose values span lines, which its own rule
// judges whole ([Shape.Problem]) in place of a credential's one-line rule.
func (s Shape) Multiline() bool { return s == ShapeES256Key || s == ShapeCertificateAuthority }

// Problem is the sentence for a pasted value this shape refuses, "" for one it
// takes. noun names the field's value ("provisioner token"). It never quotes
// the value, only where the characters it refuses are.
func (s Shape) Problem(noun, value string) string {
	if s == ShapeCertificateAuthority {
		if _, _, err := ParseCertificateAuthority(value); err != nil {
			return fmt.Sprintf("This %s is not a certificate authority the engine minted: it is a P-256 "+
				"private key's PEM block, then its certificate's. Leave the field to the engine, which "+
				"mints one on Save.", noun)
		}
		return ""
	}
	if s == ShapeES256Key {
		if _, err := parseES256(value); err != nil {
			return fmt.Sprintf("This %s is not a key the engine minted: it is one PKCS#8 PEM "+
				"block holding a P-256 private key. Leave the field to the engine, which mints one "+
				"on Save.", noun)
		}
		return ""
	}
	if s != ShapeOpaqueToken {
		return ""
	}
	var at []int
	for i, r := range []rune(value) {
		if !opaqueTokenRune(r) {
			at = append(at, i+1)
		}
	}
	if len(at) == 0 {
		return ""
	}
	which := "character " + strconv.Itoa(at[0])
	if len(at) > 1 {
		which = "characters " + words.AndMore(at, 3)
	}
	return fmt.Sprintf("This %s holds a character a token never has (%s): a token is letters, "+
		"digits, hyphens and underscores alone. Paste only the token's value, from Copy on the page "+
		"that shows it.", noun, which)
}

// opaqueTokenRune reports a character of [ShapeOpaqueToken]'s alphabet.
func opaqueTokenRune(r rune) bool {
	return r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// Mint produces a value of this shape.
//
// `token` is crypto/rand's own text encoding: 26 base32 characters over 128
// bits of entropy, URL-safe and safe to paste into a third-party app's header
// field, which is where a shared token ends up.
func (s Shape) Mint(token func() string) (string, error) {
	switch s {
	case ShapeSigningKey:
		return whsec.Mint()
	case ShapeCertificateAuthority:
		return GenerateCertificateAuthority(CertificateAuthorityName(), time.Now())
	case ShapeES256Key:
		return mintES256()
	}
	return token(), nil
}
