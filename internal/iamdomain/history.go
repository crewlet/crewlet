package iamdomain

import (
	"fmt"
	"slices"
)

// HistoryClass is which retention horizon one authentication-trail row falls
// under.
//
// TWO, and they are two because they answer different questions. "Who
// suspended this person, and when" is an audit somebody asks a year later, and
// in several jurisdictions is one they are required to be able to ask. "Who
// signed in on Tuesday" is operational: it answers an investigation that is
// days old and is a location-adjacent record of a person's working hours for
// as long as it is kept, so keeping it as long as the first would be storing
// more about people than the company has a reason to.
//
// THE CLASS IS DERIVED FROM THE OP, never carried on the record. A writer that
// stated its own class could put a suspension in the short horizon, and the
// row would simply be gone when somebody went looking.
type HistoryClass string

const (
	// ClassChange is a change to WHO SOMEBODY IS or WHAT THEY MAY DO: an
	// enrolment, a claim, a status move, a revocation, a removal.
	ClassChange HistoryClass = "change"

	// ClassSession is a session beginning or ending.
	ClassSession HistoryClass = "session"
)

// HistoryClasses are the two.
var HistoryClasses = []HistoryClass{ClassChange, ClassSession}

// Valid reports whether a class off the wire is one this build knows.
func (c HistoryClass) Valid() bool { return slices.Contains(HistoryClasses, c) }

// historyClass is the op's class, and the ABSENCE of an op from this table is
// how a record says it writes no history row at all.
//
// ONE TABLE BESIDE THE ENUM rather than a switch in the applier, because two
// readers need it — the applier writing the row and the sweep resolving a
// horizon — and a switch in one of them is a classification the other cannot
// see. [ClassOf]'s exhaustiveness is a build failure, so an op added without a
// class fails here rather than silently writing no trail.
var historyClass = map[OpKind]HistoryClass{
	OpInvite:  ClassChange,
	OpClaim:   ClassChange,
	OpRedeem:  ClassChange,
	OpRelease: ClassChange,
	OpEnrol:   ClassChange,
	OpUpdate:  ClassChange,
	OpStatus:  ClassChange,
	OpRevoke:  ClassChange,
	OpRemove:  ClassChange,

	OpOpen:  ClassSession,
	OpClose: ClassSession,

	// A BOOTSTRAP IS A CHANGE, and it is the sharpest one in the domain:
	// it is how somebody who had no account acquired the company's first
	// administrator. It belongs in the horizon that keeps things.
	OpBootstrap: ClassChange,
}

// unrecorded are the ops that write NO history row, stated as a list rather
// than as the absence of a map entry.
//
// The difference matters because [ClassOf] must be able to tell "this op is
// deliberately silent" from "somebody added an op and forgot to classify it",
// and a missing map key is both. Each of these writes no trail for a reason
// of its own:
//
//   - a BARRIER writes no row anywhere, by definition;
//   - a SWEEP's whole job is deleting trail rows, and a trail row per sweep
//     would make the table grow from being swept;
//   - an EVICTION and a GENERATION are facts about the LOG rather than about
//     anybody's identity, and the state log has its own operator surface for
//     both.
var unrecorded = []OpKind{OpSweep, OpBarrier, OpEviction, OpGeneration}

// ClassOf is the history class one op's row falls under, and whether it writes
// one at all.
func ClassOf(op OpKind) (HistoryClass, bool) {
	class, ok := historyClass[op]
	return class, ok
}

// CheckHistoryClasses reports an op this build knows that is neither
// classified nor deliberately silent.
//
// EXPORTED so the domain's own suite can assert it over [OpKinds] without
// reaching into the map — and so the failure names the op rather than being a
// count that goes green when one omission is fixed and another appears. It is
// two-sided: an op that is BOTH classified and listed as silent is a
// contradiction the applier would resolve by writing a row the sweep's
// horizons do not cover.
func CheckHistoryClasses() error {
	for _, op := range OpKinds {
		_, classified := historyClass[op]
		silent := slices.Contains(unrecorded, op)
		switch {
		case classified && silent:
			return fmt.Errorf("iamdomain: op %q is classified as %q and also "+
				"listed as writing no history row — the applier would write a "+
				"row the sweep's horizons were told does not exist", op,
				historyClass[op])
		case !classified && !silent:
			return fmt.Errorf("iamdomain: op %q has no history class and is "+
				"not listed as silent — an unclassified op writes no "+
				"authentication trail at all, which is indistinguishable from "+
				"one that was never meant to", op)
		}
	}
	for op := range historyClass {
		if !slices.Contains(OpKinds, op) {
			return fmt.Errorf("iamdomain: op %q is classified and is not an op "+
				"this build writes — the entry is a horizon assigned to "+
				"nothing", op)
		}
	}
	for _, op := range unrecorded {
		if !slices.Contains(OpKinds, op) {
			return fmt.Errorf("iamdomain: op %q is listed as silent and is not "+
				"an op this build writes", op)
		}
	}
	return nil
}
