package pages

import "slices"

// THE TABLE INVENTORIES, and what each list is for.
//
// Three readers derive from these and none of them can see the others: the
// domain's own Tables() map, which the framework turns into a scrub list, an
// identity claim and a local sweep; the completeness audit, which asserts a
// replay from zero reproduces every one of them; and the applier, which is
// the only writer of any of them.

// ReproducibleTables is every table a record's payload must be able to
// rebuild.
//
// THE COMPLETENESS AUDIT AS A LIST, so a table added later without a payload
// field to fill it goes red rather than shipping. It is what a replay from
// zero into an empty database has to end up with.
//
// THREE TABLES ARE DELIBERATELY ABSENT, and they are the log's own machinery
// rather than state a record reproduces: the operation ledger, the deferred
// records and their scope index. The framework's checkpoint is absent for the
// same reason and is not this domain's to name.
var ReproducibleTables = []string{
	// The five object tables.
	//
	// `pages_titles` is one of them rather than machinery: a title is an
	// ADDRESS, and a node whose titles were not rebuilt would let a
	// second page take a name the first already holds.
	"pages_containers", "pages_heads", "pages_titles",
	"pages_revisions", "pages_comments",

	// The two exploded child tables. Each exists because a FILTER reads
	// it: a collection inside a document cannot be indexed, and a query
	// that decodes every row's document is a full scan wearing an index's
	// name.
	"pages_labels", "pages_watchers",

	// The history, which is what a card and a digest render from.
	"pages_history",

	// And the two fleet-wide tables: the deletion marker, which is how a
	// node that was away tells a page that never existed from one that was
	// deliberately destroyed, and the eviction fence.
	"pages_deletions", "pages_evictions",

	// The reanchor's audit row.
	"pages_log_generations",
}

// MachineryTables are the log's own, excluded from the audit and from the
// identity claim, and scrubbed out of every donated snapshot.
//
// A DONOR'S OPERATION LEDGER IS THE SHARPEST OF THEM: an adopted peer's ops
// table would let this node resolve its own ambiguous publish against somebody
// else's history, which is a write reported as landed that never happened.
var MachineryTables = []string{
	"pages_ops", "pages_log_deferred", "pages_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
