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
	// Applied is how far this node's identity applier has committed,
	// packed — what a token's mint position is compared against.
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

	// Grants and Colleague are what the MINT carried, a subset of what the
	// owner held then; Epoch is the owner's revocation epoch at the mint,
	// and Generation the company's session generation at it.
	Grants     []iam.Grant
	Colleague  iam.Colleague
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

	// Grants and Colleague are the owner's CURRENT ones, which is what
	// makes demoting a person demote every token they minted.
	Grants    []iam.Grant
	Colleague iam.Colleague

	// Epoch is the owner's CURRENT revocation epoch.
	Epoch uint64

	// Seat and SeatAt are the owner's binding, for the caller to resolve
	// through the chart exactly as a session's is.
	Seat   string
	SeatAt uint64
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

	switch {
	case row.Lag > stall:
		return TokenCheck{TokenUnknown, fmt.Sprintf("the identity applier is "+
			"%s behind, past the %s stall grace", row.Lag, stall)}
	case row.Deferred:
		return TokenCheck{TokenUnknown, "this node holds a record it cannot " +
			"decode whose scope covers this token's owner"}
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
	case !row.RevokedAt.IsZero() && !now.Before(row.RevokedAt):
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

// EffectiveGrants is what a valid token carries on this request: the grants
// its mint named that its owner STILL holds.
//
// RE-EVALUATED PER REQUEST, never frozen at the mint, so taking a grant away
// from a person takes it away from every token they made. The deployment's
// ceiling is the caller's to apply on top, as it is for every credential.
func (row TokenRow) EffectiveGrants() []iam.Grant {
	out := make([]iam.Grant, 0, len(row.Grants))
	for _, g := range row.Grants {
		if slices.Contains(row.Owner.Grants, g) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// EffectiveColleague is how far a valid token reaches into the company's work:
// the narrower of what its mint named and what its owner reaches now.
//
// THE ZERO VALUE IS THE CLOSED END, as it is wherever a reach is stored.
func (row TokenRow) EffectiveColleague() iam.Colleague {
	minted, owner := row.Colleague, row.Owner.Colleague
	if minted == "" {
		minted = iam.ColleagueNone
	}
	if owner == "" {
		owner = iam.ColleagueNone
	}
	return minted.AtMost(owner)
}
