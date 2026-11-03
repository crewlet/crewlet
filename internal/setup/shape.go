package setup

import (
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/config"
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

	// ShapeGoogleServiceAccount is a Google Cloud service account's email,
	// `<id>@<project>.iam.gserviceaccount.com`, as config holds it.
	ShapeGoogleServiceAccount Shape = "google_service_account"

	// ShapeGoogleProvider is a workload identity provider's full resource
	// name, as config holds it.
	ShapeGoogleProvider Shape = "google_workload_provider"
)

// Problem is the sentence for a pasted value this shape refuses, "" for one it
// takes. noun names the field's value ("provisioner token"). It never quotes
// the value, only where the characters it refuses are.
func (s Shape) Problem(noun, value string) string {
	switch s {
	case ShapeGoogleServiceAccount:
		if _, ok := config.GoogleCloudServiceAccount(value); ok {
			return ""
		}
		return fmt.Sprintf("This %s is not a service account's email: it reads "+
			"<id>@<project>.iam.gserviceaccount.com, as the service account's page shows it.", noun)
	case ShapeGoogleProvider:
		if config.GoogleCloudProvider(value) {
			return ""
		}
		return fmt.Sprintf("This %s is not a workload identity provider's full name: it reads "+
			"projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>, "+
			"as the last line the Cloud Shell block prints.", noun)
	case ShapeOpaqueToken:
	default:
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
//
// A VENDOR-ISSUED IDENTIFIER IS NEVER MINTED: a service account or a provider
// Crewlet made up would name nothing at Google.
func (s Shape) Mint(token func() string) (string, error) {
	switch s {
	case ShapeSigningKey:
		return whsec.Mint()
	case ShapeGoogleServiceAccount, ShapeGoogleProvider:
		return "", fmt.Errorf("setup: a %s value is issued by the vendor and cannot be minted", s)
	default:
		return token(), nil
	}
}
