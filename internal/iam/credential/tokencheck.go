package credential

import (
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHETHER A PRESENTED MACHINE TOKEN IS GOOD, as a table over values.
//
// The guard's third credential arm reads ONE snapshot of this node's own rows
// and hands them here; nothing below reads anything, so every cell is a case a
// suite can state without a database — the same division internal/iam/session
// draws for a cookie, for its reason: a rule exercised only through a store is
// a rule nobody re-reads.
//
// # Three answers, and the third is the point
//
// A token is VALID, REFUSED (401 — this node knows it is no good) or UNKNOWN
// (503 — this node cannot tell). The third is never folded into the second: a
// node behind the identity log that answered 401 to a token it had not yet
// seen minted would teach every pipeline in the company that its credential
// is broken, and one past the stall grace would do it for every token at once.

// TokenAnswer is one of the three.
type TokenAnswer string

const (
	// TokenValid is: this request proceeds, as the token's owner.
	TokenValid TokenAnswer = "valid"

	// TokenRefused is 401. Reached only when this node KNOWS.
	TokenRefused TokenAnswer = "refused"

	// TokenUnknown is 503. A pipeline retries it and keeps its credential.
	TokenUnknown TokenAnswer = "unknown"
)

// TokenRow is everything one presented token is checked against, read in ONE
// snapshot of this node's own replicated rows.
//
// ONE READ, for internal/iam/session's reason: a revocation landing between a
// credential read and an owner read would produce a verdict that never
// existed at any instant.
type TokenRow struct {
	// Applied is how far this node's identity applier has reached — every
	// record through it that writes a row committed, and every barrier
	// decoded — packed: what a token's mint position is compared against.
	Applied uint64

	// Lag is how far behind the log this node is, and Deferred whether it
	// holds a record it could not decode whose scope covers the owner.
	// FACTS, NOT A VERDICT: the threshold is the caller's to supply.
	Lag      time.Duration
	Deferred bool

	// Found reports a credential row under the token's id at all, and
	// IsToken that its method is a machine token — a password's id
	// presented as a token is a token that does not exist.
	Found   bool
	IsToken bool

	// Verifier is what the secret is checked against.
	Verifier string

	ExpiresAt time.Time
	RevokedAt time.Time

	// Grants are what the MINT carried, a subset of what the owner held
	// then; Epoch is the owner's revocation epoch at the mint, and
	// Generation the company's session generation at it.
	Grants     []iam.Grant
	Epoch      uint64
	Generation uint64

	// FleetGeneration is the company's session generation as this node
	// holds it NOW, read in the same snapshot as the row.
	FleetGeneration uint64

	// Owner is who the token acts as, as this node holds them NOW.
	Owner TokenOwner
}

// TokenOwner is a token's owner as this node holds them now.
type TokenOwner struct {
	Found bool

	ID    string
	Kind  iam.Kind
	Stage iam.Stage
	Login string

	// Grants are the owner's CURRENT ones, which is what makes demoting a
	// person demote every token they minted.
	Grants []iam.Grant

	// Epoch is the owner's CURRENT revocation epoch.
	Epoch uint64

	// Seat is the owner's binding, for the caller to resolve through the
	// running organisation exactly as a session's is.
	Seat string
}

// TokenCheck is one token's verdict.
type TokenCheck struct {
	Answer TokenAnswer

	// Detail says which fact decided, for a log line. NEVER sent to the
	// caller: telling somebody whether a token was revoked, expired or
	// never existed tells whoever holds it the same.
	Detail string
}

// CheckToken decides one presented token against the rows this node holds.
//
// # The order is the contract
//
// COVERAGE FIRST: a node past stall, or holding an undecodable record about
// this owner, cannot vouch for anything below it. THEN THE ROW, three-valued
// on the mint position the token carries. THEN THE SECRET, in constant time.
// And only then the facts that END a good token — revoked, expired, the owner
// gone, suspended, signed out everywhere since the mint, or every credential in
// the company invalidated since it — because each of those is a statement
// about a credential this node has established is real.
func CheckToken(presented Token, row TokenRow, now time.Time,
	stall time.Duration) TokenCheck {

	if check, covered := coverage(row, stall); !covered {
		return check
	}
	if !row.Found {
		if row.Applied >= presented.Position {
			return TokenCheck{TokenRefused, "this node covers the mint's " +
				"position and holds no credential under that id"}
		}
		// THE MINT HAS NOT REACHED THIS NODE, which is the one absence
		// that is a wait rather than an answer.
		return TokenCheck{TokenUnknown, "this node has not applied the " +
			"token's mint yet"}
	}
	// THE SECRET IS CHECKED EVEN FOR A ROW OF ANOTHER METHOD, so the time
	// a refusal takes says nothing about what the id names.
	matched := VerifyToken(row.Verifier, presented)
	switch {
	case !row.IsToken:
		return TokenCheck{TokenRefused, "the id names a credential that is " +
			"not a machine token"}
	case !matched:
		return TokenCheck{TokenRefused, "the secret does not verify"}
	}
	return ending(row, now)
}

// CheckHeld decides one token by its rows alone, for a caller that holds the
// token's NAME rather than its secret: a view ticket minted through the token,
// whose secret the minting request already proved, decided again on the
// artifact origin, where the secret never travels.
//
// THE SAME TABLE BUT THE TWO FACTS ONLY A BEARER HAS. Coverage and every fact
// that ends a good token are [CheckToken]'s, cell for cell. The secret is not
// checked, since nothing here presents one. And an ABSENT row is UNKNOWN,
// never refused: without the mint position a presented token carries, a node
// cannot tell a credential it has not applied yet from one that was never
// there, and a refusal it could not stand behind would end a frame whose
// credential is fine.
func CheckHeld(row TokenRow, now time.Time, stall time.Duration) TokenCheck {
	if check, covered := coverage(row, stall); !covered {
		return check
	}
	switch {
	case !row.Found:
		return TokenCheck{TokenUnknown, "this node holds no credential under " +
			"that id, and without the token's mint position it cannot say " +
			"whether it has applied it"}
	case !row.IsToken:
		return TokenCheck{TokenRefused, "the id names a credential that is " +
			"not a machine token"}
	}
	return ending(row, now)
}

// coverage is whether this node can vouch for the rows at all: a node past the
// stall grace, or holding a record it could not decode whose scope covers the
// owner, cannot, and every answer below it would be a guess.
func coverage(row TokenRow, stall time.Duration) (TokenCheck, bool) {
	switch {
	case row.Lag > stall:
		return TokenCheck{TokenUnknown, fmt.Sprintf("this node is %s behind "+
			"on identity changes, over the %s limit", row.Lag, stall)}, false
	case row.Deferred:
		return TokenCheck{TokenUnknown, "this node has an identity change it " +
			"can't read that may affect this token's owner"}, false
	}
	return TokenCheck{}, true
}

// ending is the facts that end a token this node has established is real, in
// the order a log line should name the first of them.
func ending(row TokenRow, now time.Time) TokenCheck {
	switch {
	case !row.RevokedAt.IsZero():
		// A REVOCATION IS A FACT AND NOT A SCHEDULE. The stamp is the
		// REVOKING node's clock, kept for the listing's "withdrawn on the
		// 3rd"; compared against THIS node's clock, a token revoked by a
		// node running ahead went on working here for as long as the two
		// disagreed — after its record had applied, on the one gesture
		// that has to end a credential the instant it lands. Set is
		// revoked, whatever instant it names.
		return TokenCheck{TokenRefused, "the token was revoked"}
	case !now.Before(row.ExpiresAt):
		// A ZERO EXPIRY LANDS HERE TOO, which is the point: a token with
		// none is not one this engine mints, and the zero time is before
		// every instant, so the one reading that would fail open —
		// "never" — is not a reading this comparison can make.
		return TokenCheck{TokenRefused, "the token has expired, or carries " +
			"no expiry"}
	case !row.Owner.Found:
		return TokenCheck{TokenRefused, "the token's owner is gone"}
	case !row.Owner.Stage.MayAct():
		return TokenCheck{TokenRefused, fmt.Sprintf("the token's owner is %q, "+
			"and only %q may act", row.Owner.Stage, iam.StageActive)}
	case row.Owner.Epoch > row.Epoch:
		// WHAT MAKES OFFBOARDING COMPLETE: every session the owner held
		// ended when their epoch moved, and so does every token they
		// minted before it.
		return TokenCheck{TokenRefused, "the owner's revocation epoch has " +
			"moved past the one the token was minted at"}
	case row.FleetGeneration > row.Generation:
		// WHAT MAKES A RESTORE SAFE: a backup taken before a token was
		// revoked restores it unrevoked, and nothing can say which ones
		// were — so the generation the restore runbook bumps ends every
		// bearer credential issued before it, a token as much as a
		// cookie.
		return TokenCheck{TokenRefused, "the company's session generation " +
			"has moved past the one the token was minted at"}
	}
	return TokenCheck{TokenValid, ""}
}

// EffectiveGrants is what a valid token carries on this request: in
// [iam.AllGrants] order, every permission the mint named that its owner STILL
// holds, both sides read through [iam.Expand].
//
// RE-EVALUATED PER REQUEST, never frozen at the mint, so taking a permission
// away from a person takes it away from every token they made. Through
// [iam.Expand] on both sides, because a token minted [admin] carries every app
// its owner keeps when admin is taken away from them, and a token minted
// [worklet] by an administrator who never ticked the app separately still
// carries it. A name this build does not know grants nothing, here as
// everywhere. The node's admin posture is the caller's to apply on top, as it
// is for every credential.
//
// ADMIN IS CARRIED LIKE ANY OTHER PERMISSION. What keeps a token off the
// gestures that need a person present is not a permission it cannot hold: it
// is internal/authz's `presence` on the row, which refuses any request a token
// presented there, and internal/iamdomain's refusal of an administrative
// identity record written through one.
func (row TokenRow) EffectiveGrants() []iam.Grant {
	minted, owner := iam.Expand(row.Grants), iam.Expand(row.Owner.Grants)
	out := make([]iam.Grant, 0, len(iam.AllGrants))
	for _, g := range iam.AllGrants {
		if slices.Contains(minted, g) && slices.Contains(owner, g) {
			out = append(out, g)
		}
	}
	return out
}
