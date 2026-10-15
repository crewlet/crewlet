package artifactsdomain

import "slices"

// THE TABLE INVENTORIES. Three readers derive from these and none of them can
// see the others: the domain's own Tables() map, which the framework turns into
// a scrub list, an identity claim and a local sweep; the replay cases, which
// assert a replay from zero reproduces every one of them; and the applier,
// which is the only writer of any of them.

// ReproducibleTables is every table a record's payload must be able to
// rebuild: what a replay from zero into an empty database has to end up with,
// byte for byte, on every data node.
var ReproducibleTables = []string{
	// The object tables.
	"artifacts", "artifact_source_claims", "artifact_versions",

	// The two tables that NAME OBJECTS, each declared in
	// internal/objstore/references so the collector never deletes a file a
	// retained version or a live asset still names.
	"artifact_objects", "artifact_assets",

	// Access and per-person state.
	"artifact_shares", "artifact_pins", "artifact_reads",

	// What happened, and the counters the stamped quotas are enforced
	// against.
	"artifact_history", "artifact_usage",

	// The folder tree: the folders, the shares their top-level folders
	// hold, and what happened to the tree (an artifact's own place is its
	// `folder_id` column).
	"artifact_folders", "artifact_folder_shares", "artifact_folder_history",

	// The adopted brand: the adoption's snapshot, the files it serves (the
	// third table that NAMES OBJECTS, declared in
	// internal/objstore/references), and every adoption and clear.
	"artifact_brand", "artifact_brand_files", "artifact_brand_history",

	// The gate tables, which outlive the records that wrote them, and the
	// reanchor's audit row.
	"artifacts_deletions", "artifacts_evictions", "artifacts_log_generations",
}

// MachineryTables are the log's own: the operation ledger, which TRAVELS in a
// donated snapshot (Divergent), and the deferred records and their scope
// index, this node's own verdict about bytes it could not decode (Local).
var MachineryTables = []string{
	"artifacts_ops", "artifacts_log_deferred", "artifacts_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
