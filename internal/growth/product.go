package growth

import (
	"crypto/subtle"
	"errors"
	"slices"
)

// Call is one call of a growth tool by a seat's turn: ID names a call made at
// once, which a product records it under ([Record.Call]); a held call names
// the held call a person answered and who answered it instead.
type Call struct {
	ID, Handle, Turn, Tool string
	Args                   map[string]any
	HeldCall, Approver     string
}

// Hold is a held call as its product prepared it: the digest of the exact
// vendor operations and the target's before-state it binds to, the identity of
// what it decides (delegated.Subject), and what a person reads. Ask is what
// the access log records of the ask: its connection, account, target,
// digests, changes and money; the engine adds the seat, turn, tool, class and
// held call.
type Hold struct {
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

// ErrMoved is an approved call whose target changed since a person was asked:
// nothing was sent.
var ErrMoved = errors.New("growth: the target changed since the call was approved")

// BindingHolds reports whether binding, as a person approved it, is fresh,
// compared in constant time.
func BindingHolds(approved, fresh string) bool {
	return len(approved) == len(fresh) && subtle.ConstantTimeCompare([]byte(approved), []byte(fresh)) == 1
}
