package growth

import (
	"crypto/subtle"
	"errors"
	"slices"
	"time"
)

// ApplyBound cuts everything a product does under an act's apply lease (the
// target's read and the write, at minute-long request timeouts), so nothing
// acts once the lease could lapse; internal/engine's lease lives past it.
const ApplyBound = 2 * time.Minute

// Call is one call of a growth tool by a seat's turn: ID names a call made at
// once, which a product records it under ([Record.Call]); a held call names
// the held call a person answered and who answered it instead.
type Call struct {
	ID, Handle, Turn, Tool string
	Args                   map[string]any
	HeldCall, Approver     string
}

// EngineActor is who the engine's own answer to a held call names: its lapse
// (internal/engine's sweep), which is no administrator's.
const EngineActor = "engine"

// Denial is the step a denied held call records and who answered it: an
// administrator's deny, or the engine's lapse, which names nobody.
func (c Call) Denial() (Step, string) {
	if c.Approver == "" || c.Approver == EngineActor {
		return StepLapsed, ""
	}
	return StepDenied, c.Approver
}

// Hold is a write as its product prepared it: the class it comes to against
// its target as read now ([Classify]), the digest of the exact vendor
// operations and the target's before-state it binds to, and for a held call
// the identity of what it decides (delegated.Subject) and what a person reads.
// Ask is what the access log records of the ask: its connection, account,
// target, digests, changes and money; the engine adds the seat, turn, tool,
// class and held call.
type Hold struct {
	Class                               Class
	Binding, Subject, Question, Context string
	Ask                                 Record
}

// RefusalKind is why a product refused a call before any vendor was reached.
type RefusalKind string

// The refusals.
const (
	RefusedForbidden   RefusalKind = "forbidden"
	RefusedInvalid     RefusalKind = "invalid"
	RefusedNotFound    RefusalKind = "not_found"
	RefusedConflict    RefusalKind = "conflict"
	RefusedUnavailable RefusalKind = "unavailable"
)

// Valid reports whether k is a refusal this build knows.
func (k RefusalKind) Valid() bool {
	return slices.Contains([]RefusalKind{RefusedForbidden, RefusedInvalid, RefusedNotFound, RefusedConflict,
		RefusedUnavailable}, k)
}

// Refusal is a call refused before any vendor was reached, in words the agent
// acts on.
type Refusal struct {
	Kind RefusalKind
	Said string
}

func (r *Refusal) Error() string { return r.Said }

// Refuse is a refusal of kind, said.
func Refuse(kind RefusalKind, said string) error { return &Refusal{Kind: kind, Said: said} }

// ErrActHeld is the one failure of an act's lease (a product's Leaser) that
// means another act on the account holds it: a provider wraps it, and a
// product answers it as unavailable, to call again. Any other failure is the
// lease store's, answered as itself: told to call again, a caller would retry
// a refusal no retry changes.
var ErrActHeld = errors.New("growth: another act holds the account's lease")

// ErrMoved is an approved call whose target changed since a person was asked:
// nothing was sent.
var ErrMoved = errors.New("growth: the target changed since the call was approved")

// BindingHolds reports whether binding, as a person approved it, is fresh,
// compared in constant time.
func BindingHolds(approved, fresh string) bool {
	return len(approved) == len(fresh) && subtle.ConstantTimeCompare([]byte(approved), []byte(fresh)) == 1
}
