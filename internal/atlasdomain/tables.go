package atlasdomain

import "slices"

// THE TABLE INVENTORIES, which the domain's Tables(), the replay cases and the
// applier all read.

// ReproducibleTables is every table a replay from zero must rebuild byte for
// byte: the entries and their claims, the purge markers, the node gate's table
// and the reanchor's audit.
var ReproducibleTables = []string{
	"atlas_entries", "atlas_claims", "atlas_purges", "atlas_evictions", "atlas_log_generations",
}

// MachineryTables are the log's own: the ledger, which travels in a donated
// snapshot, and the deferred records with their scope index, this node's own.
var MachineryTables = []string{"atlas_ops", "atlas_log_deferred", "atlas_log_deferred_scope"}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
