package artifactdata

import "slices"

// THE TABLE INVENTORIES. The domain's Tables(), the replay cases and the
// applier each derive from these and none of them can see the others.

// ReproducibleTables is every table a record's payload must be able to
// rebuild, byte for byte, on every data node.
var ReproducibleTables = []string{
	// The store, its collections and interned paths.
	"artifact_data_stores", "artifact_data_collections", "artifact_data_paths",

	// The documents and their field index.
	"artifact_data_docs", "artifact_data_fields",

	// The hot history and the documents each row touched.
	"artifact_data_writes", "artifact_data_touches",

	// The company's counters the stamped limits are enforced against.
	"artifact_data_usage",

	// The gate tables and the reanchor's audit row.
	"artifactdata_deletions", "artifactdata_evictions", "artifactdata_log_generations",
}

// MachineryTables are the log's own: the operation ledger (Divergent) and the
// deferred records and their scope index (Local).
var MachineryTables = []string{
	"artifactdata_ops", "artifactdata_log_deferred", "artifactdata_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
