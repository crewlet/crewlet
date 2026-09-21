package chart

import "slices"

// THE TABLE INVENTORIES, and what each list is for.
//
// Three readers derive from these and none of them can see the others: the
// domain's own Tables() map, which the framework turns into a scrub list, an
// identity claim and a local sweep; the completeness audit, which asserts a
// replay from zero reproduces every one of them; and the applier, which is the
// only writer of any of them.

// ReproducibleTables is every table a record's payload must be able to rebuild.
//
// THE COMPLETENESS AUDIT AS A LIST, so a table added later without a payload
// field to fill it goes red rather than shipping. It is what a replay from zero
// into an empty database has to end up with.
//
// THREE TABLES ARE DELIBERATELY ABSENT, and they are the log's own machinery
// rather than state a record reproduces: the operation ledger, the deferred
// records and their scope index. The framework's checkpoint is absent for the
// same reason and is not this domain's to name.
var ReproducibleTables = []string{
	// The two object tables. Both carry the object's full encoded
	// document, so a field a newer build wrote round-trips through a
	// durable row rather than being dropped by the build that read it.
	"chart_units", "chart_seats",

	// The two AUTHORED EDGE tables.
	//
	// An edge in an org chart has two ends and the document authors
	// exactly one of them: a seat states what it manages, and a unit
	// states who leads it. The other end — who manages me, which units do
	// I lead — is DERIVED, and is read as often as the authored end. That
	// is why each is a table with an index in the unauthored direction
	// rather than a collection inside its owner's document: a collection
	// cannot be indexed, and a query that decodes every row's document to
	// answer "who manages alice" is a full scan wearing an index's name.
	"chart_manages", "chart_leads",

	// The history, which is what a card, a digest and an operator asking
	// "when did this team change hands" all render from.
	"chart_history",

	// And the import ledger: which company revision produced which
	// position on this log.
	"chart_import_ledger",
}

// MachineryTables are the log's own, excluded from the audit and from the
// identity claim, and scrubbed out of every donated snapshot.
//
// A DONOR'S OPERATION LEDGER IS THE SHARPEST OF THEM: an adopted peer's ops
// table would let this node resolve its own ambiguous publish against somebody
// else's history, which is a write reported as landed that never happened.
var MachineryTables = []string{
	"chart_ops", "chart_log_deferred", "chart_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
