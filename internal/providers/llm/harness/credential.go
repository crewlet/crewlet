package harness

import (
	"strings"
	"time"
)

// CredentialKind is which of a CLI's two pasted credentials a value is, by the
// prefix its vendor gives it ([harnessprofile.Profile.TokenPrefix],
// [harnessprofile.Profile.APIKeyPrefix]).
//
// It exists for the refusal of a credential pasted into the wrong slot: an API
// key stored where the headless token belongs is handed to the CLI as a token,
// and the vendor's answer ("invalid bearer token") names neither the slot nor
// the paste. Told apart before any call is made, the person is told which
// slot the value belongs in.
type CredentialKind string

const (
	// CredentialToken is the vendor's headless subscription token.
	CredentialToken CredentialKind = "token"
	// CredentialAPIKey is the vendor's metered API key.
	CredentialAPIKey CredentialKind = "api_key"
	// CredentialUnknown is a value that starts with neither declared prefix,
	// or one the profile declares none for. It is no refusal: a vendor that
	// changes its format, or a gateway's token, starts with something else,
	// and only a call can say whether it signs in.
	CredentialUnknown CredentialKind = "unknown"
)

// Valid reports whether k is a kind this package names.
func (k CredentialKind) Valid() bool {
	switch k {
	case CredentialToken, CredentialAPIKey, CredentialUnknown:
		return true
	}
	return false
}

// CredentialKind is which credential value is, by the prefixes this entry's
// profile declares. The profile refuses two prefixes where one begins the
// other, so a value matches one of them at most.
func (p *Provider) CredentialKind(value string) CredentialKind {
	switch {
	case p.profile.TokenPrefix != "" && strings.HasPrefix(value, p.profile.TokenPrefix):
		return CredentialToken
	case p.profile.APIKeyPrefix != "" && strings.HasPrefix(value, p.profile.APIKeyPrefix):
		return CredentialAPIKey
	}
	return CredentialUnknown
}

// CredentialHint is the hint of the credential this entry's auth mode hands
// its CLI, read off the environment the child is given ([childEnv]), or ""
// when the mode hands it none: the headless token under subscription (none
// when the entry signs in with credential files), the key under api-key, and
// under inherit-env whichever of the two the engine's own environment
// forwards, the token first, as [Provider.signIn] names them.
//
// NOTHING UNDER auth.mode env. That mode's sign-in is a set of variables (a
// cloud provider's switch beside its region and keys, or a gateway's address
// beside its token), and no one of them is the credential: a hint of
// CLAUDE_CODE_USE_BEDROCK's "1" would describe nothing, and a hint of one
// variable picked from the set would read as the whole sign-in.
//
// Computed on the node that answers, from the value it resolved, and never
// stored, logged or published: see [Provider.CredentialHintOf].
func (p *Provider) CredentialHint() string {
	var names []string
	switch p.AuthMode() {
	case AuthSubscription:
		names = []string{p.profile.TokenEnv}
	case AuthAPIKey:
		names = []string{p.profile.APIKeyEnv}
	case AuthInheritEnv:
		names = []string{p.profile.TokenEnv, p.profile.APIKeyEnv}
	}
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	for _, name := range names {
		if value := env[name]; name != "" && strings.TrimSpace(value) != "" {
			return p.CredentialHintOf(value)
		}
	}
	return ""
}

// hintTail is how many of a credential's last characters its hint shows, and
// hintHidden how many characters a hint must leave out before it shows any.
//
// THE HINT TELLS TWO CREDENTIALS APART, AND NEVER SPELLS ONE. Its prefix says
// which kind it is and its last four which one, which is what a person
// comparing the vendor's console with the page needs. A value short enough
// that its prefix and its last four are most of it is no credential any
// vendor issues, and showing them would show the value; it gets the ellipsis
// alone.
const (
	hintTail   = 4
	hintHidden = 8
)

// CredentialHintOf is the hint of one value of this entry's credential: the
// declared prefix it starts with ([Provider.CredentialKind]), else its first
// three characters, then "…" and its last four; "" for no value.
//
// A VALUE, for the caller that has one this provider never resolved: a
// candidate credential tested before it is stored is hinted in the answer
// that says whether it signed in, by the prefixes of the entry it is for.
func (p *Provider) CredentialHintOf(value string) string {
	var prefix string
	switch p.CredentialKind(value) {
	case CredentialToken:
		prefix = p.profile.TokenPrefix
	case CredentialAPIKey:
		prefix = p.profile.APIKeyPrefix
	}
	return HintOf(prefix, value)
}

// HintOf is the hint of value under a declared prefix ([Provider.CredentialHint]):
// the prefix where the value starts with it, else its first three characters,
// then "…" and its last four; "…" alone for a value too short to leave enough
// of it unsaid, and empty for none. A function of its own so a candidate value
// is hinted by the same rule before any provider holds it.
func HintOf(prefix, value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	head := runes[:min(3, len(runes))]
	if prefix != "" && strings.HasPrefix(value, prefix) {
		head = []rune(prefix)
	}
	if len(runes) < len(head)+hintTail+hintHidden {
		return "…"
	}
	return string(head) + "…" + string(runes[len(runes)-hintTail:])
}

// TokenLifetime is how long a headless token this CLI mints lasts, as its
// vendor documents it ([harnessprofile.Profile.TokenLifetimeDays]); zero is
// unknown. The token is opaque, so a caller computes its expiry from when it
// was stored, and says that it did.
func (p *Provider) TokenLifetime() time.Duration {
	return time.Duration(p.profile.TokenLifetimeDays) * 24 * time.Hour
}
