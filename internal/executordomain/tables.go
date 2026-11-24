package executordomain

import "slices"

// THE TABLE INVENTORIES, which three readers derive from without seeing each
// other: the domain's Tables() map (the framework's scrub list, identity claim
// and local sweep), the replay cases, and the applier, their only writer.

// ReproducibleTables is every table a record's payload must rebuild: what a
// replay from zero into an empty database ends up with, byte for byte, on every
// node.
var ReproducibleTables = []string{
	// The sessions; the tombstones that keep an ended or removed turn from
	// being captured again; the removals that keep a record decided before a
	// seat's removal from recreating its turns.
	"executor_sessions", "executor_session_tombs", "executor_seat_removals",

	// The other conversations an ended turn served, each reading its row.
	"executor_session_serves",

	// The media objects a turn's row names, which go with its object.
	"executor_session_media",

	// The node gate's table and the reanchor's audit row.
	"executor_evictions", "executor_log_generations",
}

// MachineryTables are the log's own: the operation ledger, which TRAVELS in a
// donated snapshot (Divergent), and the deferred records and their scope
// index, this node's own verdict about bytes it could not decode (Local).
var MachineryTables = []string{
	"executor_ops", "executor_log_deferred", "executor_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
