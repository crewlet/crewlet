package gitlet

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

	// A repository's viewed key, the 32 random bytes its persons' viewed marks
	// digest each path and file under: written once by the viewed_key record,
	// removed by the repository's purge.
	"code_viewed_keys",

	// A repository's pushes by UTC day and quarter-hour and by pusher and UTC day: kept with
	// code_pushes' rows by the applies and re-derived by the applier (apply_activity.go).
	"code_push_days", "code_pusher_days",

	// The packs a record retired, each kept from the collector until a
	// retire_sweep record removes it past RetireGrace.
	"code_packs_retired",

	// The objects a forced update or a deletion moved a ref off, each kept
	// until a push record of its repository forgets it past PruneGrace or a
	// purge removes it.
	"code_ref_drops",

	// A repository's changes, their authors, every revision of a head, and the
	// body (its own row, off the one every head move rewrites): written on the
	// repository's subject and removed with it, each change's revisions,
	// authors and body before its row.
	"code_changes", "code_change_authors", "code_revisions", "code_change_bodies",

	// A change's conversation — reviewers, comments, participants, reviews and
	// findings, counters, and the facts and mergeability the repository's home
	// recorded of its revisions: written on the CHANGE's subject alone, removed
	// with the repository's changes, each change's before its row.
	"code_reviewers", "code_comments", "code_participants", "code_conversations",
	"code_reviews", "code_findings", "code_finding_texts", "code_revision_facts", "code_mergeability",

	// A repository's checks — each attempt of each check of each commit, and a
	// failed run's log — and what its home dispatched: each target's checks
	// file as last read and the file each change head was dispatched under.
	// Written on the repository's CHECKS subject alone, each apply a join
	// (check_dispatched also writes the attempts it queues); attempts removed
	// past CheckHistory by those records and the code-retention duty's
	// checks_sweep, and all of it with the repository by its purge.
	"code_checks", "code_check_logs", "code_check_targets", "code_check_dispatches",

	// A repository's merge queues — enqueued entries, the candidates a queue
	// builds, landings on protected branches, each branch's green row and the
	// tip work it is owed: written on the repository's subject, removed by its
	// purge.
	"code_queue", "code_candidates", "code_landings", "code_green", "code_tips_due",

	// A repository's published chain and the objects its layers are stored
	// in — written by the compaction records that carry a layer, each
	// replacing the chain's top, and removed whole by its purge.
	"code_layers", "code_layer_parts",

	// A seat's pushes on its company day, for the decide's
	// pushes_per_seat_day: counted by the push apply, each agent entry on the
	// day its decide stamped, so every node counts alike with no clock of its
	// own.
	"code_seat_days",

	// How many repositories of each kind the log holds, every status short
	// of purged, for a tool's create of a project (repo_limit): counted in
	// by a create's apply and out by the purge's.
	"code_repo_kinds",

	// The changes of the repositories in use by state, a row for each owner and visibility:
	// kept with code_changes by the applies and re-derived by the applier (apply_counts.go).
	"code_change_tallies",

	// The repositories by owner, visibility, kind and status: kept by the lifecycle applies and
	// re-derived by the applier (apply_repotallies.go).
	"code_repo_tallies",

	// A person's marks over their code notices (marks.go): written on the
	// person's subject alone, each a gesture applied to the row its
	// predecessor left at the record's broker instant. Not a repository's, so
	// no purge removes it.
	"code_person_marks",

	// The files a person marked viewed on a change, by keyed digests
	// (viewed.go): written on the person's subject alone. Not a repository's,
	// so no purge removes them; the repository's purge deletes the key, after
	// which they name no file once the log is trimmed past the key's record.
	"code_viewed_files",

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
