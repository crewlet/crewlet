package assistantdomain

import "slices"

// THE TABLE INVENTORIES, the one list three readers that cannot see each other
// derive from: the domain's Tables() map (the framework's scrub list, identity
// claim and local sweep), the replay cases, and the applier, the only writer.

// ReproducibleTables is every table a record's payload must be able to
// rebuild: what a replay from zero into an empty database has to end up with,
// byte for byte, on every node.
var ReproducibleTables = []string{
	// The conversation and everything in it.
	"assistant_conversations", "assistant_turns", "assistant_messages",
	"assistant_steps", "assistant_cards", "assistant_proposals",
	"assistant_parked", "assistant_audit_reads", "assistant_files",

	// The gate tables, which outlive the records that wrote them, and the
	// reanchor's audit row.
	"assistant_deletions", "assistant_erasures", "assistant_evictions",
	"assistant_log_generations",
}

// conversationTables are the tables a purge empties of one conversation's
// rows, each keyed by `conversation_id`: one list for the purge and the
// erasure alike, so a table added here is one both remove.
var conversationTables = []string{
	"assistant_turns", "assistant_messages", "assistant_steps", "assistant_cards",
	"assistant_proposals", "assistant_parked", "assistant_audit_reads",
	"assistant_files",
}

// MachineryTables are the log's own: the operation ledger, which TRAVELS in a
// donated snapshot (Divergent), and the deferred records and their scope
// index, this node's own verdict about bytes it could not decode (Local).
var MachineryTables = []string{
	"assistant_ops", "assistant_log_deferred", "assistant_log_deferred_scope",
}

// Reproducible reports whether a table is one a record must rebuild.
func Reproducible(table string) bool { return slices.Contains(ReproducibleTables, table) }
