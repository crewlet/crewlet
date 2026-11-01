package assistantdomain

import "slices"

// THE TABLE INVENTORIES. Three readers derive from these and none of them can
// see the others: the domain's own Tables() map, which the framework turns into
// a scrub list, an identity claim and a local sweep; the replay cases, which
// assert a replay from zero reproduces every one of them; and the applier,
// which is the only writer of any of them.

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
// rows, each keyed by `conversation_id` except the conversation's own: ONE
// LIST, read by the purge and the erasure alike, so a table added here is a
// table both remove.
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
