package tracker

import "slices"

// The two exported lists a durable-schema test drives.
//
// Both exist for the same reason: a fact about the schema that is written
// twice drifts, and both of these were written three times in the design this
// replaces — the struct, the DDL and the applier's statement for the first,
// and the migration, the snapshot's scrub list and the identity claim for the
// second.

// SpendColumns are the seven counters and the derived sort key, in the order
// the applier writes them.
//
// ONE LIST, read by the struct, the DDL and the applier's own statement. Eight
// column names typed out three times is three chances to add the ninth to two
// of them — and the failure is silent: a counter nothing increments reads zero
// for ever, which looks exactly like a task nobody has worked on.
//
// `spend_tokens` is DERIVED — input plus output — rather than transmitted,
// because it is the sort key and a sort key that can disagree with the columns
// it summarises is a board that orders by a number nobody can reproduce.
var SpendColumns = []string{
	"spend_turns", "spend_rounds", "spend_input", "spend_output",
	"spend_cache_read", "spend_cache_write", "spend_wall_ms", "spend_tokens",
}

// ReproducibleTables is every table a record's payload must be able to rebuild.
//
// THE COMPLETENESS AUDIT AS A LIST, so a table added later without a payload
// field to fill it goes red rather than shipping. It is what a replay from
// zero into an empty database has to end up with — and the failure it exists
// to prevent is precise: the design this replaces would have ended a replay
// with a history table, a notification table and NO TASKS, because every one
// of its record's caps was a routing cap.
//
// FIVE TABLES ARE DELIBERATELY ABSENT, and they are the log's own machinery
// rather than state a record reproduces: the framework's checkpoint, the
// operation ledger, the deferred records, their scope index and the adoption
// row. A sixth, the binary vector table, is DERIVED — a pure function of a
// table that is itself outside the identity claim.
var ReproducibleTables = []string{
	// The nineteen object tables.
	"tracker_tasks", "tracker_comments", "tracker_body_revisions",
	"tracker_task_keys", "tracker_projects", "tracker_sprints",
	"tracker_counters", "tracker_tagsets", "tracker_catalogues",
	"tracker_tags", "tracker_types", "tracker_fields",
	"tracker_field_options", "tracker_views", "tracker_goals",
	"tracker_persons", "tracker_rank_orders", "tracker_log_generations",
	"tracker_evictions",

	// The fourteen exploded child tables. Each one exists because a FILTER
	// reads it: a collection inside a document cannot be indexed, and a
	// query that decodes every row's document is a full scan wearing an
	// index's name.
	"tracker_task_closure", "tracker_task_sprints", "tracker_collaborators",
	"tracker_watchers", "tracker_task_tags", "tracker_field_values",
	"tracker_relations", "tracker_task_deps", "tracker_task_dependents",
	"tracker_references", "tracker_checklist_items", "tracker_goal_owners",
	"tracker_goal_targets", "tracker_goal_target_refs",

	// The four history tables.
	"tracker_history", "tracker_turns", "tracker_status_spans",
	"tracker_notifications",

	// And the deletion marker, which is how a node that was away tells a
	// task that never existed from one that was deliberately destroyed.
	"tracker_deletions",
}

// MachineryTables are the log's own, excluded from the audit and from the
// identity claim, and scrubbed out of every donated snapshot.
//
// A DONOR'S OPERATION LEDGER IS THE SHARPEST OF THEM: an adopted peer's ops
// table would let this node resolve its own ambiguous publish against somebody
// else's history, which is a write reported as landed that never happened.
var MachineryTables = []string{
	"tracker_ops", "tracker_log_deferred", "tracker_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool {
	return slices.Contains(ReproducibleTables, table)
}
