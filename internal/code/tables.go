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
	// included), the pack bundles a push stored, the push history, the
	// protection rules and the path owners — the last two written whole by
	// every record that carries the repository's settings.
	"code_repos", "code_names", "code_refs", "code_packs", "code_pushes",
	"code_protections", "code_path_owners",

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

	// A repository's changes, each with who wrote it, every revision of its
	// head and its body — a row of its own, kept off the row every head
	// move rewrites — written by the records on the repository's own
	// subject, like its refs, and removed with the repository, a change's
	// revisions, authors and body before its row.
	"code_changes", "code_change_authors", "code_revisions", "code_change_bodies",

	// A change's conversation — who is asked to review it, its comments, who
	// takes part, its reviews and their findings, its counters, and the facts
	// and the mergeability its repository's home recorded of its revisions —
	// written by the records on the CHANGE's own subject, which no record on
	// the repository's writes, and removed with the repository's changes, each
	// change's before its row.
	"code_reviewers", "code_comments", "code_participants", "code_conversations",
	"code_reviews", "code_findings", "code_finding_texts", "code_revision_facts", "code_mergeability",

	// A repository's checks — each attempt of each check of each commit, and
	// the log a failed run kept — written by the records on the repository's
	// CHECKS subject alone, each apply a join, and removed past CheckHistory
	// by those records and by the code-retention duty's checks_sweep, and
	// with the repository by its purge. Beside them what the repository's
	// home dispatched: each target's checks file as the home last read it,
	// and the file each change head was dispatched under — written by the
	// check_dispatched records on the same subject, each apply a join, and
	// removed with the repository by its purge.
	"code_checks", "code_check_logs", "code_check_targets", "code_check_dispatches",

	// A repository's merge queues — each entry a change's enqueue made, the
	// candidates a pre_land queue builds, the landings on its protected
	// branches and each branch's green row — written by the records on the
	// repository's own subject, beside its pushes and its changes'
	// lifecycles, and removed with the repository by its purge.
	"code_queue", "code_candidates", "code_landings", "code_green",

	// A repository's published chain and the objects its layers are stored
	// in — written by the compaction records that carry a layer, each
	// replacing the chain's top, and removed whole by its purge.
	"code_layers", "code_layer_parts",

	// A seat's pushes counted on its company day, one row a seat and day,
	// for the decide's pushes_per_seat_day: the push apply's own counter,
	// a function of the push records alone — each agent entry counted on
	// the day its decide stamped — so a replay rebuilds it, and every node
	// counts alike with no clock of its own.
	"code_seat_days",

	// How many repositories of each kind the log holds, every status short
	// of purged, for a tool's create of a project (repo_limit): counted in
	// by a create's apply and out by the purge's that removes the row, so a
	// replay rebuilds it from those records alone.
	"code_repo_kinds",

	// A person's marks over their code notices — how far they have read on
	// this log, what they marked out of order or put off, which reasons are
	// theirs — written by the records on the person's own subject alone,
	// each a gesture applied to the row its predecessor left, at the
	// record's broker instant (marks.go): state a replay rebuilds like any
	// other. Not a repository's, so no purge removes it.
	"code_person_marks",

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
