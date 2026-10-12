// Package references is the one list of replicated tables whose rows name
// objects of the object store — ADR-0026's cross-package half.
//
// # Why a package of one list
//
// The collector deletes every object no row names, so a table that names
// objects and is missing from this list is a table whose files are deleted a
// day after they were written. The consumer that owns such a table cannot
// hold the list (the collector would have to import every consumer), and the
// object store cannot hold it (every consumer imports the object store). So
// each consumer declares its table beside its own schema, and this package
// collects the declarations — and its test holds the list against the
// replicated schema in BOTH directions: a column named `object` in a table not
// listed here fails the build, and so does a listed table the schema does not
// have, or one whose key column no index leads with.
//
// # The one input, not one of several
//
// The collector, its audit and the backup build their statements FROM this
// list ([objstore.ReferenceTable.ObjectsAmong],
// [objstore.ReferenceTable.ReferencesAfter]); none carries a query of its own.
// A domain supplies only what a declaration cannot — a barrier on its log and
// a read of its rows (collect.Estate) — and the engine's own test builds the
// passes from this list against the estates it hands them, so a table
// declared in a domain the engine reads no estate of fails the build rather
// than the boot.
package references

import (
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/tracker"
)

// All is every replicated table whose rows keep objects alive.
var All = []objstore.ReferenceTable{
	tracker.FileObjectReferences,
}

// ObjectColumn is the name every referencing table gives its key column, and
// what the schema gate looks for. One name, so a new table cannot name its
// objects something the gate does not recognise.
const ObjectColumn = "object"
