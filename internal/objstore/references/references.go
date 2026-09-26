// Package references is the one list of replicated tables whose rows name
// chunks of the object store — ADR-0019's cross-package half.
//
// # Why a package of one list
//
// The collector deletes every chunk no row names, so a table that names chunks
// and is missing from this list is a table whose files are deleted a day after
// they were written. The consumer that owns such a table cannot hold the list
// (the collector would have to import every consumer), and the object store
// cannot hold it (every consumer imports the object store). So each consumer
// declares its table beside its own schema, and this package collects the
// declarations — and its test holds the list against the replicated schema in
// BOTH directions: a column named `chunk` in a table not listed here fails the
// build, and so does a listed table the schema does not have.
//
// # The one input, not one of several
//
// The collector, the repair and the backup build their statements FROM this
// list ([objstore.ReferenceTable.Chunks]); none carries a query of its own. A
// domain supplies only what a declaration cannot — a barrier on its log and a
// read of its rows (upkeep.Estate) — and the engine's own test builds the
// passes from this list against the estates it hands them, so a table
// declared in a domain the engine reads no estate of fails the build rather
// than the boot.
package references

import (
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/tracker"
)

// All is every replicated table whose rows keep chunks alive.
var All = []objstore.ReferenceTable{
	tracker.FileChunkReferences,
}

// ChunkColumn is the name every referencing table gives its chunk column, and
// what the schema gate looks for. One name, so a new table cannot name its
// chunks something the gate does not recognise.
const ChunkColumn = "chunk"
