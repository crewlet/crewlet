package setup

import (
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/es256"
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

	// ShapeSSHCertificateAuthority is an SSH certificate authority the
	// engine signs short-lived user certificates with: an unencrypted
	// OpenSSH Ed25519 private key ([GenerateSSHCertificateAuthority]).
	// Servers are shown only its public line.
	ShapeSSHCertificateAuthority Shape = "ssh_certificate_authority"

	// ShapeGoogleProject is a Google Cloud project's ID that a service
	// account's email can name, as the console shows it under the name.
	ShapeGoogleProject Shape = "google_project"

	// ShapeGoogleProvider is a workload identity provider's full resource
	// name, as config holds it.
	ShapeGoogleProvider Shape = "google_workload_provider"

	// ShapeUUID is a vendor identifier or secret written as a UUID, such as
	// a Scaleway Organization ID or secret key: 32 hexadecimal digits in
	// groups of 8, 4, 4, 4 and 12, joined by hyphens.
	ShapeUUID Shape = "uuid"

	// ShapeScalewayAccessKey is a Scaleway API key's access key: SCW
	// followed by 17 capital letters or digits.
	ShapeScalewayAccessKey Shape = "scaleway_access_key"

	// ShapeMetaToken is a Meta access token, a system user's as Business
	// Settings generates it: EAA and letters and digits, issued by Meta and
	// never minted.
	ShapeMetaToken Shape = "meta_token"

	// ShapeAppleAdsKey is an Apple Ads API user's private key, the P-256 key
	// whose public half the person uploaded: one PEM block, SEC 1 as
	// `openssl ecparam` writes it or PKCS #8, pasted whole, never minted.
	ShapeAppleAdsKey Shape = "apple_ads_key"

	// ShapeAppleAdsID is an Apple Ads API user's client or team ID:
	// SEARCHADS. and an identifier.
	ShapeAppleAdsID Shape = "apple_ads_id"
)

// The patterns the identifier shapes hold a value to.
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go);
// their bounded repeats compile to programs holding every count, about 25 KB.
var (
	uuidShape = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	})
	scalewayAccessKeyShape = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^SCW[A-Z0-9]{17}$`)
	})
	metaTokenShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^EAA[A-Za-z0-9]{30,}$`) })
)

// Multiline reports a shape whose values span lines, which its own rule
// judges whole ([Shape.Problem]) in place of a credential's one-line rule.
func (s Shape) Multiline() bool {
	return s == ShapeES256Key || s == ShapeCertificateAuthority || s == ShapeSSHCertificateAuthority ||
		s == ShapeAppleAdsKey
}

// Accept is the file types a pasted value of this shape comes in, as a file
// picker's accept list takes them, "" for a value that is never a file.
func (s Shape) Accept() string {
	switch s {
	case ShapeAppleAdsKey, ShapeES256Key:
		return ".pem,.key"
	default:
		return ""
	}
}

// Canonical is value as it is judged and stored: a pasted private key's PEM
// with the line breaks a one-line box or a copy flattened written back
// ([es256.NormalisePEM]), so the key sealed is one every reader parses. Every
// other shape keeps value as it came.
func (s Shape) Canonical(value string) string {
	switch s {
	case ShapeES256Key, ShapeAppleAdsKey:
		return es256.NormalisePEM(value)
	default:
		return value
	}
}

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
	switch s {
	case ShapeSSHCertificateAuthority:
		if _, err := ParseSSHCertificateAuthority(value); err != nil {
			return fmt.Sprintf("This %s is not an SSH certificate authority the engine can sign with: it is "+
				"one unencrypted OpenSSH private key holding an Ed25519 key. Leave the field to the engine, "+
				"which mints one on Save.", noun)
		}
		return ""
	case ShapeES256Key:
		if _, err := parseES256(value); err != nil {
			return fmt.Sprintf("This %s is not a key the engine minted: it is one PKCS#8 PEM "+
				"block holding a P-256 private key. Leave the field to the engine, which mints one "+
				"on Save.", noun)
		}
		return ""
	case ShapeAppleAdsKey:
		if _, err := es256.ParseKey(value); err != nil {
			return fmt.Sprintf("This %s is not a P-256 private key: paste the whole file openssl wrote, from "+
				"BEGIN EC PRIVATE KEY to END EC PRIVATE KEY.", noun)
		}
		return ""
	case ShapeAppleAdsID:
		if config.AppleAdsID(value) {
			return ""
		}
		return fmt.Sprintf("This %s is not an Apple Ads ID: it is SEARCHADS. followed by an identifier, as "+
			"Account Settings, API shows it.", noun)
	case ShapeUUID:
		if uuidShape().MatchString(value) {
			return ""
		}
		return fmt.Sprintf("This %s is not a UUID: it is 32 hexadecimal digits in groups of 8, 4, 4, 4 "+
			"and 12, joined by hyphens. Paste only the value, from the page that shows it.", noun)
	case ShapeScalewayAccessKey:
		if scalewayAccessKeyShape().MatchString(value) {
			return ""
		}
		return fmt.Sprintf("This %s is not a Scaleway access key: it is SCW followed by 17 capital "+
			"letters or digits, 20 characters in all. Paste the access key shown beside the secret key.", noun)
	case ShapeMetaToken:
		if metaTokenShape().MatchString(value) {
			return ""
		}
		return fmt.Sprintf("This %s is not a Meta access token: it is EAA followed by letters and digits. "+
			"Paste the token Business Settings, System users, Generate new token shows once.", noun)
	case ShapeGoogleProject:
		if config.GoogleCloudIdentityProject(value) {
			return ""
		}
		return fmt.Sprintf("This %s is not a Google Cloud project ID: 6 to 30 lowercase letters, digits and "+
			"hyphens, starting with a letter and not ending with a hyphen. Google shows it under the project's "+
			"name, and it can differ from the name.", noun)
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
// A VENDOR-ISSUED IDENTIFIER IS NEVER MINTED: a value of [ShapeUUID],
// [ShapeScalewayAccessKey], [ShapeGoogleProject], [ShapeGoogleProvider],
// [ShapeAppleAdsKey] or [ShapeAppleAdsID] names something the vendor
// or the person made, and one Crewlet made up would name nothing.
func (s Shape) Mint(token func() string) (string, error) {
	switch s {
	case ShapeSigningKey:
		return whsec.Mint()
	case ShapeCertificateAuthority:
		return GenerateCertificateAuthority(CertificateAuthorityName(), time.Now())
	case ShapeSSHCertificateAuthority:
		return GenerateSSHCertificateAuthority()
	case ShapeES256Key:
		return mintES256()
	case ShapeMetaToken:
		// MINTED BY META ALONE: one Crewlet made up would sign nothing in.
		return "", fmt.Errorf("setup: a %s value is issued by the vendor and cannot be minted", s)
	case ShapeUUID, ShapeScalewayAccessKey, ShapeGoogleProject, ShapeGoogleProvider,
		ShapeAppleAdsKey, ShapeAppleAdsID:
		return "", fmt.Errorf("setup: a %s value is issued by the vendor and cannot be minted", s)
	default:
		return token(), nil
	}
}
