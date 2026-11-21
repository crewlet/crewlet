package secrets

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// The shared-token rule, and the one place that decides what a usable one is.
//
// Datadog's Webhooks (FIXED headers) and Confluence Cloud (URL only) offer no
// signature, so a shared token IS the whole authentication, and an accepted
// delivery drives a turn on content the sender chose. A token has no shape
// ([whsec] holds the signing-secret rule), so the only check is that there is
// enough of it to be worth guessing; the dashboard mints 26 base32 characters
// ([MinSharedTokenChars]).
//
// Enforced in TWO places, for [whsec]'s reason: config refuses a short
// literal, and the webhook edge refuses a short RESOLVED value, so a `${VAR}`
// pointing at a weak token is not accepted where a literal would be refused.

// MinSharedTokenChars is the shortest shared token these routes accept.
//
// 26 characters, which is what crypto/rand.Text() produces: base32 over 130
// bits, and the value the dashboard's own mint already hands out. The floor is
// therefore "at least as strong as what this engine gives you" rather than a
// number argued from first principles — an operator who took the offered
// token is already above it, and one who typed a word is the case this exists
// for.
//
// Measured in CHARACTERS rather than bytes: what an operator pastes is
// printable text of some unknown encoding, and a byte count would quietly
// pass a 26-byte value that is nine characters of UTF-8.
const MinSharedTokenChars = 26

// CheckSharedToken reports why a shared token cannot authenticate a delivery,
// or nil.
//
// The caller decides what to do with that — config turns it into a validation
// problem naming the field, and the webhook edge refuses the route — but
// neither gets to decide what a usable token IS.
//
// AN EMPTY TOKEN IS NOT THIS FUNCTION'S BUSINESS. "Not configured" and
// "configured with something too weak" lead to different messages and are
// distinguished by every caller before it reaches here, so this refuses an
// empty string with the same reason as any other short one rather than
// inventing a third answer.
func CheckSharedToken(token string) error {
	if strings.ContainsFunc(token, unicode.IsSpace) {
		return fmt.Errorf("a shared token cannot contain whitespace: it is sent " +
			"verbatim in a header or a URL, and a space there is a value the " +
			"provider never sends back the same way")
	}
	// THE FLOOR IS NAMED AND THE LENGTH GIVEN IS NOT. This message reaches
	// places the token must never reach: a config refusal's detail and its
	// problems, answered to whoever submitted a document whose masked
	// credentials were restored from the stored revision, and a log line.
	// A value's length is a fact about the value, and it narrows a guess at
	// exactly the credential this rule calls too short to be safe.
	if len([]rune(token)) < MinSharedTokenChars {
		return fmt.Errorf(
			"a shared token must be at least %d characters: it is the whole "+
				"authentication for this route, because the provider signs "+
				"nothing, so anything short enough to guess is a way to wake "+
				"a seat with content somebody else chose. The dashboard mints "+
				"one of exactly %d",
			MinSharedTokenChars, MinSharedTokenChars)
	}
	return nil
}

// CheckOperatorToken reports why a Tier A `api.auth.tokens` value cannot be a
// credential, or nil.
//
// The arithmetic is [CheckSharedToken]'s, so the two cannot drift on "how long
// is long enough". The sentence differs: this token authenticates the OPERATOR
// (config, secret store, fleet), and CheckSharedToken's "the provider signs
// nothing" would be about a route they are not configuring.
func CheckOperatorToken(token string) error {
	if strings.ContainsFunc(token, unicode.IsSpace) {
		return errors.New("an `api.auth.tokens` token can't contain spaces")
	}
	// THE FLOOR IS NAMED AND THE LENGTH GIVEN IS NOT, for the reason
	// CheckSharedToken states: this message reaches a config refusal, a
	// /config response and a log line, and a value's length narrows a
	// guess at exactly the credential this rule calls too short.
	if len([]rune(token)) < MinSharedTokenChars {
		return fmt.Errorf(
			"an `api.auth.tokens` token must be at least %d characters. "+
				"Generate one with `crewlet secrets keygen`",
			MinSharedTokenChars)
	}
	return nil
}
