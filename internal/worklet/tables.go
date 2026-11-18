package worklet

import "slices"

// The exported lists a durable-schema test drives: a fact about the schema
// written in more than one place drifts, so each is written once.

// SpendColumns are the counters and the derived sort key, in the order the
// applier writes them.
//
// One list, read by the struct, the DDL and the applier's statement: a column
// added to only some of them fails silently, a counter reading zero for ever
// like a task nobody worked on (ADR-0022). `spend_tokens` is DERIVED (input
// plus output), not transmitted: a sort key that can disagree with the
// columns it summarises orders the board by a number nobody can reproduce.
var SpendColumns = []string{
	"spend_turns", "spend_rounds", "spend_input", "spend_output",
	"spend_cache_read", "spend_cache_write", "spend_wall_ms", "spend_tokens",
	"spend_workers", "spend_sent_back",
}

// ReproducibleTables is every table a record's payload must be able to rebuild.
//
// The completeness audit as a list: what a replay from zero into an empty
// database must end up with, so a table added without a payload field to fill
// it goes red rather than shipping. The log's own machinery is deliberately
// absent ([MachineryTables]), as are the framework's tables (`statelog_cursor`,
// `statelog_anchor`, `statelog_ops_lost`), which are not this domain's to name.
var ReproducibleTables = []string{
	// The object tables.
	"tracker_tasks", "tracker_comments", "tracker_body_revisions",
	"tracker_task_keys", "tracker_projects",
	"tracker_counters", "tracker_tagsets", "tracker_catalogues",
	"tracker_tags", "tracker_views",
	"tracker_persons", "tracker_rank_orders", "tracker_log_generations",
	"tracker_evictions", "tracker_files", "tracker_rosters",

	// The exploded child tables, each read by a filter or a figure: a
	// collection inside a document cannot be indexed.
	"tracker_task_closure",
	"tracker_collaborators",
	"tracker_watchers", "tracker_task_tags", "tracker_field_values",
	"tracker_relations", "tracker_task_deps", "tracker_task_dependents",
	"tracker_references", "tracker_checklist_items",
	// The roster's places, which `sort=assignee` joins on the seat's key.
	"tracker_seat_places",

	// The history tables.
	"tracker_history", "tracker_turns",
	"tracker_notifications",

	// The deletion marker: how a node that was away tells a task that never
	// existed from one that was deliberately destroyed.
	"tracker_deletions",
}

// MachineryTables are the log's own, excluded from the audit and from the
// identity claim.
//
// They do not all travel. The deferred record and its scope are this node's
// verdict about bytes this build could not decode, scrubbed from every donated
// snapshot. The operation ledger (tracker_ops) travels: every node's applier
// writes the same rows, and an adopter without them cannot tell a first
// attempt from a retry of what the donor applied ([statelog.Domain.OpsTable]).
var MachineryTables = []string{
	"tracker_ops", "tracker_log_deferred", "tracker_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
