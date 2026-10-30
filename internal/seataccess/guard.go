package seataccess

import (
	"slices"

	"github.com/crewlet/crewlet/internal/config"
)

// THE DEFENCE IN DEPTH under the document's rules.
//
// The document's rules already refuse a seat variable that a company block or
// another seat names, on submit, and report one on a stored document. A pass
// does not trust that alone: a stored document from before the rule, or one an
// operator wrote around the dashboard, reaches the pass all the same, and the
// pass is the one about to WRITE. So every sync refuses to seal into a name
// anything but the seat itself references, and nothing forgets one.
//
// The test is "named anywhere but in this seat's own role entry". The engine
// derives a seat's sealed names and binds them to its tools itself (D4), so no
// company block, unit or other seat has a legitimate reason to name one; the
// seat's own entry may, only while a stored document still carries a retired
// credential field the migration has not rewritten.

// SharedSinks is every sealed name of handle's at cat's kind that something
// outside the seat's own role entry references, sorted. Non-empty refuses the
// seat's sync.
func SharedSinks(cat Catalog, handle string, refs config.VariableReferences) []string {
	var out []string
	for _, name := range SinkNames(cat, handle) {
		if refs.OutsideSeat(name, handle) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Forgettable is names less every one that something outside handle's own
// role entry references: what a teardown or an off may clear from the sealed
// store without taking a company credential, or another seat's, with it.
//
// A name it holds back stays sealed and is the operator's to replace; the
// sync's finding for the shared name says so.
func Forgettable(names []string, handle string, refs config.VariableReferences) []string {
	var out []string
	for _, name := range names {
		if refs.OutsideSeat(name, handle) == 0 {
			out = append(out, name)
		}
	}
	return out
}
