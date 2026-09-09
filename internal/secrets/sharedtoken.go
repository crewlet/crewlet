package secrets

import (
	"fmt"
	"strings"
	"unicode"
)

// The shared-token rule, and the one place that decides what a usable one is.
//
// # What a shared token is doing here
//
// Two inbound routes cannot verify a signature, because their providers do
// not offer one: Datadog's Webhooks integration attaches headers with FIXED
// values only, so nothing varies with the payload to compute an HMAC over,
// and Confluence Cloud attaches nothing at all and delivers only what was
// written into its URL. For both, a shared token IS the authentication —
// the whole of it — and an accepted delivery wakes a seat and drives a turn
// on content the sender chose.
//
// A signing secret has a shape that gives it its strength, and [whsec] is
// where that rule lives. A shared token has no shape at all: it is whatever
// two sides agree on, so the only property that can be checked is that there
// is enough of it to be worth guessing. Nothing else in the request is
// evidence of anything.
//
// # Why it is checked rather than trusted
//
// The dashboard mints one with crypto/rand and it is 26 characters of base32,
// which is where [MinSharedTokenChars] comes from. Every other way a value
// reaches these fields — a hand-edited company document, `PATCH /config`, a
// `${VAR}` resolved at the edge — accepted anything at all, so a one-character
// token was a valid configuration and this tree's own tests used one.
//
// The rule is enforced in TWO places for the reason [whsec]'s doc gives, in
// the same words, because it is the same failure: config refuses a literal
// that is too short, and the webhook edge refuses a RESOLVED value that is,
// so a `${VAR}` pointing at a weak token cannot be silently accepted where a
// literal would have been refused. Written once and checked in one of the two,
// the reference is the hole.

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
	if n := len([]rune(token)); n < MinSharedTokenChars {
		return fmt.Errorf(
			"a shared token must be at least %d characters and this one is %d: "+
				"it is the whole authentication for this route, because the "+
				"provider signs nothing, so anything short enough to guess is "+
				"a way to wake a seat with content somebody else chose. "+
				"The dashboard mints one of exactly %d",
			MinSharedTokenChars, n, MinSharedTokenChars)
	}
	return nil
}
