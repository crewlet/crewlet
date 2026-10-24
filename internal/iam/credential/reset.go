package credential

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"time"
)

// PASSWORD RESET LINKS: what an administrator hands somebody who cannot sign
// in, and what is stored for it — and the same credential in its other use, the
// FIRST PASSWORD LINK an administrator's create issues the person it creates,
// which sets the password they never had through the same spend.
//
// A link is `<id>.<secret>` in the fragment of the dashboard's reset screen —
// the invitation link's shape, for the invitation link's reasons: the id is
// the credential's own and in the clear, so checking one is a keyed read of
// one row, and the SECRET is the half that authenticates, 32 bytes — of
// crypto/rand for a reset, of a MAC under the company's key for a first
// password link, whose create's retry has to hand back the same one — that
// never reach a path or a query string.
//
// SHA-256 AT REST, for the machine token's reason ([TokenVerifier]): a value
// this engine minted has no dictionary to grind, so a memory-hard digest would
// buy nothing. The id and a prefix of its own are inside the digest, so a
// verifier copied onto another row verifies nothing there, and no machine
// token's value could ever verify against a reset's.

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
// company stays good — an invitation, and a created person's first password
// link: ONE WEEK.
//
// A HUNDRED AND SIXTY-EIGHT HOURS, and the number is the SECURITY HORIZON
// rather than a convenience: the link — its secret — is a bearer credential
// sitting in somebody's mailbox, so the window is how long a compromised
// mailbox yields an account. A week survives somebody being away — a
// newcomer is sent the link before their first day, and may not read it until
// then — without making the link a standing way in, and an administrator
// whose link aged out issues another in one call.
//
// ONE CONSTANT FOR BOTH, because the two are the same person's position: an
// invitee who chooses their own login, and a person an administrator created
// who chooses only their password. It was the invitation surface's own, and
// the first link would have had to restate it.
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
