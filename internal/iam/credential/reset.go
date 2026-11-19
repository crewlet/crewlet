package credential

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"time"
)

// PASSWORD RESET LINKS, and the FIRST PASSWORD LINK a create issues.
//
// A link is `<id>.<secret>` in the fragment of the reset screen: the id is the
// credential's own, in the clear (a keyed read of one row); the SECRET
// authenticates, 32 bytes of crypto/rand, or for a first password link a MAC
// under the company's key, since the create's retry must hand back the same one.
//
// SHA-256 AT REST, for the machine token's reason ([TokenVerifier]): a minted
// value has no dictionary to grind. The id and a prefix of its own are inside
// the digest, so a verifier copied onto another row verifies nothing there, and
// no machine token's value verifies against a reset's.

// ResetLinkLifetime is how long a reset link stays good: TWENTY-FOUR HOURS.
//
// The link is a credential that sets a password without the old one, and it
// travels out of band — an administrator sends it by chat or mail, which is
// where a link sits unread and gets forwarded. So it lives long enough to
// reach somebody the next working day and no longer: a reset is for somebody
// locked out today. Somebody who does not work here yet is given
// [EnrolmentLinkLifetime] instead.
const ResetLinkLifetime = 24 * time.Hour

// EnrolmentLinkLifetime is how long a link that brings somebody INTO the
// company stays good (an invitation, a created person's first password link):
// ONE WEEK.
//
// The SECURITY HORIZON, not a convenience: the link is a bearer credential in
// somebody's mailbox, so the window is how long a compromised mailbox yields an
// account. A week survives a newcomer reading it on their first day without
// making it a standing way in; an expired link is reissued in one call. ONE
// CONSTANT FOR BOTH: they are the same person's position.
const EnrolmentLinkLifetime = 168 * time.Hour

// resetPrefix separates a reset's verifier from a machine token's, which is
// formed the same way under [TokenPrefix].
const resetPrefix = "cwl_reset_"

// NewResetSecret mints a reset link's random half: [TokenBytes] of
// crypto/rand, hex — the machine token's secret, for its reasons.
func NewResetSecret() (string, error) { return NewTokenSecret() }

// ResetVerifier is what the estate stores for a reset link: SHA-256 over the
// prefix, the credential's id and the secret.
func ResetVerifier(id, secret string) string {
	sum := sha256.Sum256([]byte(resetPrefix + id + "_" + secret))
	return hex.EncodeToString(sum[:])
}

// VerifyReset reports whether a presented secret is the one a reset link's
// stored verifier was formed from, constant-time — and false for an empty
// secret or verifier, which no link carries.
func VerifyReset(verifier, id, secret string) bool {
	if verifier == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(ResetVerifier(id, secret)),
		[]byte(verifier)) == 1
}
