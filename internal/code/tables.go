package code

import "slices"

// THE TABLE INVENTORIES. Three readers derive from these and none can see the
// others: the domain's Tables() map, which the framework turns into a scrub
// list, an identity claim and a local sweep; the completeness audit, which
// asserts a replay from zero reproduces every one; and the applier, the only
// writer of any of them.

// ReproducibleTables is every table a record's payload must be able to
// rebuild — what a replay from zero into an empty database ends up with.
//
// THREE TABLES ARE DELIBERATELY ABSENT, the log's own machinery rather than
// state a record reproduces: the operation ledger, the deferred records and
// their scope index.
var ReproducibleTables = []string{
	// A repository, its name claim, and what it holds: refs (tombstones
	// included), the pack bundles a push stored, the push history and the
	// protection rules.
	"code_repos", "code_names", "code_refs", "code_packs", "code_pushes",
	"code_protections",

	// The packs a record retired, each kept from the collector until a
	// retire_sweep record removes it past RetireGrace — every node derives
	// the same rows from the same records, so they are state a replay
	// rebuilds like any other.
	"code_packs_retired",

	// The objects a forced update or a deletion moved a ref off, each kept
	// until a push record of its repository forgets it past PruneGrace or a
	// purge removes it with the rest — written only by the records that do
	// those, so a replay rebuilds them too.
	"code_ref_drops",

	// A repository's changes, each with who wrote it and every revision of
	// its head — written by the records on the repository's own subject,
	// like its refs, and removed with the repository, a change's revisions
	// and authors before its row.
	"code_changes", "code_change_authors", "code_revisions",

	// A repository's published chain and the objects its layers are stored
	// in — written by the compaction records that carry a layer, each
	// replacing the chain's top, and removed whole by its purge.
	"code_layers", "code_layer_parts",

	// The activation fence: the configuration activation every
	// config-managed record is held to.
	"code_activation",

	// The deletion marker — how a node that was away tells a repository that
	// never existed from one that was purged — and the eviction fence.
	"code_deletions", "code_evictions",

	// The reanchor's audit row.
	"code_log_generations",
}

// MachineryTables are the log's own, excluded from the audit and from the
// identity claim.
//
// THEY DO NOT ALL TRAVEL. The deferred record and its scope are this node's
// own verdict about bytes this build could not decode, scrubbed out of every
// donated snapshot; the operation ledger travels — see
// [statelog.Domain.OpsTable].
var MachineryTables = []string{"code_ops", "code_log_deferred", "code_log_deferred_scope"}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
