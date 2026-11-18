package iamdomain

import (
	"fmt"
	"slices"
)

// HistoryClass is which retention horizon one authentication-trail row falls
// under.
//
// TWO, because they answer different questions. "Who suspended this person,
// and when" is an audit asked a year later, which some jurisdictions require.
// "Who signed in on Tuesday" answers a days-old investigation and records a
// person's working hours, so keeping it as long would store more about people
// than the company has reason to.
//
// THE CLASS IS DERIVED FROM THE OP, never carried on the record: a writer
// stating its own could put a suspension in the short horizon.
type HistoryClass string

const (
	// ClassChange is a change to WHO SOMEBODY IS or WHAT THEY MAY DO: an
	// invitation issued or cancelled, an enrolment, a login or seat change,
	// a status move, a revocation, a password set, a removal.
	ClassChange HistoryClass = "change"

	// ClassSession is a session beginning or ending.
	ClassSession HistoryClass = "session"
)

// HistoryClasses are the two.
var HistoryClasses = []HistoryClass{ClassChange, ClassSession}

// Valid reports whether a class off the wire is one this build knows.
func (c HistoryClass) Valid() bool { return slices.Contains(HistoryClasses, c) }

// historyClass is the op's class; an op absent from it writes no history row.
//
// ONE TABLE BESIDE THE ENUM, not a switch in the applier: the applier writing
// the row and the sweep resolving a horizon both read it.
// [CheckHistoryClasses] fails an op added without a class rather than let it
// silently write no trail.
var historyClass = map[OpKind]HistoryClass{
	OpInvite:   ClassChange,
	OpCancel:   ClassChange,
	OpEnrol:    ClassChange,
	OpIdentity: ClassChange,
	OpUpdate:   ClassChange,
	OpStatus:   ClassChange,
	OpRevoke:   ClassChange,
	OpPassword: ClassChange,
	OpRemove:   ClassChange,

	OpOpen:  ClassSession,
	OpClose: ClassSession,

	// AN INVALIDATION IS A CHANGE although it ends sessions: the short
	// horizon guards a record of one person's working hours, and this row
	// names nobody; ending every session in the company is exactly what
	// somebody asks an audit trail about a year later.
	OpInvalidate: ClassChange,
}

// unrecorded are the ops that write NO history row, listed rather than left
// out of the map so [CheckHistoryClasses] can tell a deliberately silent op
// from one somebody forgot to classify:
//
//   - a BARRIER writes no row anywhere, and a TRIMMED record none of this
//     domain's: the framework removes the log's anchors, its own bookkeeping;
//   - a SWEEP deletes trail rows, and a row per sweep would grow the table;
//   - an EVICTION and a GENERATION are facts about the LOG, with the state
//     log's own operator surface.
var unrecorded = []OpKind{OpSweep, OpBarrier, OpTrimmed, OpEviction, OpGeneration}

// ClassOf is the history class one op's row falls under, and whether it writes
// one at all.
func ClassOf(op OpKind) (HistoryClass, bool) {
	class, ok := historyClass[op]
	return class, ok
}

// CheckHistoryClasses reports an op this build knows that is neither
// classified nor deliberately silent.
//
// EXPORTED so the domain's suite asserts it over [OpKinds] and a failure names
// the op rather than a count. TWO-SIDED: an op both classified and silent
// would have the applier write a row the sweep's horizons do not cover.
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
