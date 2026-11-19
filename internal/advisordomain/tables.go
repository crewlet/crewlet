package advisordomain

import "slices"

// THE TABLE INVENTORIES, which the domain's Tables(), the replay cases and the
// applier all read.

// ReproducibleTables is every table a replay from zero must rebuild byte for
// byte.
var ReproducibleTables = []string{
	"advisor_keys", "advisor_findings", "advisor_finding_history", "advisor_runs",
	"advisor_class_runs", "advisor_reports", "advisor_alarm_episodes", "advisor_expiries",
	"advisor_evictions", "advisor_log_generations",
}

// MachineryTables are the log's own: the ledger, which travels in a donated
// snapshot, and the deferred records with their scope index, this node's own.
var MachineryTables = []string{"advisor_ops", "advisor_log_deferred", "advisor_log_deferred_scope"}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
