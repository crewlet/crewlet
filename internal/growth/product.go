package growth

import (
	"crypto/subtle"
	"errors"
	"slices"
)

// Call is one call of a growth tool by a seat's turn; for a held call, the
// held call a person answered and who answered it.
type Call struct {
	Handle, Turn, Tool string
	Args               map[string]any
	HeldCall, Approver string
}

// Hold is a write as its product prepared it: the class it comes to against
// its target as read now ([Classify]), the digest of the exact vendor
// operations and the target's before-state it binds to, and for a held call
// the identity of what it decides (delegated.Subject) and what a person reads.
type Hold struct {
	Class                               Class
	Binding, Subject, Question, Context string
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
