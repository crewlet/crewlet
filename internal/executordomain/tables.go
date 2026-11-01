package executordomain

import "slices"

// THE TABLE INVENTORIES. Three readers derive from these and none of them can
// see the others: the domain's own Tables() map, which the framework turns into
// a scrub list, an identity claim and a local sweep; the replay cases, which
// assert a replay from zero reproduces every one of them; and the applier,
// which is the only writer of any of them.

// ReproducibleTables is every table a record's payload must be able to
// rebuild: what a replay from zero into an empty database has to end up with,
// byte for byte, on every node.
var ReproducibleTables = []string{
	// The sessions, the tombstones that keep an ended or removed turn from
	// being captured again, and the removals that keep a removed seat's
	// turns from being recreated by a record decided before the removal.
	"executor_sessions", "executor_session_tombs", "executor_seat_removals",

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
