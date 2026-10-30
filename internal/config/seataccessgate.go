package config

import (
	"slices"
	"strings"
)

// SeatAccessMigratedFor reports whether kind's agent seats state their access
// per seat in this document: the engine's migration has moved them onto it,
// or the document was written with them. It is the gate the access pass for an
// integration reads before it acts on any seat.
//
// A DOCUMENT CARRIES IT RATHER THAN THE FLEET, so a revert to a revision
// written before the migration closes the gate again: that document's seats
// say nothing per seat, and read as off they would have every account turned
// off.
func (c *Company) SeatAccessMigratedFor(kind string) bool {
	return c != nil && slices.Contains(c.SeatAccessMigrated, kind)
}

// RefuseEngineWritten is the refusal of a write, by anybody but the engine,
// that changes a seat field only the engine writes ([EngineWrittenChanges]): a
// GitHub App's id, slug or installation, which the engine records after GitHub
// answers. Nil where the write changes none.
//
// A person or a model writing one would point an agent at an app that is not
// its own, so the field is the engine's alone (design section 2), and the
// refusal names every path the write changed.
func RefuseEngineWritten(prior, next *Company) error {
	paths := EngineWrittenChanges(prior, next)
	if len(paths) == 0 {
		return nil
	}
	return fault(nil, ErrConflict, "%s %s written by the engine once GitHub answers, and "+
		"no other writer may change them; leave them as they are",
		strings.Join(paths, ", "), pluralAre(len(paths)))
}

func pluralAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}
