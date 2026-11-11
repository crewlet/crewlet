package accessdomain

import "slices"

// THE TABLE INVENTORIES, which the domain's Tables(), the replay cases and the
// applier all read.

// ReproducibleTables is every table a replay from zero must rebuild byte for
// byte: the record tables, the seats' removals, the node gate's table and the
// reanchor's audit.
var ReproducibleTables = []string{
	"access_told", "access_seat_removals", "access_certificates", "access_commands",
	"access_verdicts", "access_evictions", "access_log_generations",
}

// RetainedTables are the tables the retention expires, each with an `at`
// column the broker stamped.
var RetainedTables = []string{
	"access_told", "access_seat_removals", "access_certificates", "access_commands", "access_verdicts",
}

// MachineryTables are the log's own: the ledger, which travels in a donated
// snapshot, and the deferred records with their scope index, this node's own.
var MachineryTables = []string{"access_ops", "access_log_deferred", "access_log_deferred_scope"}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
