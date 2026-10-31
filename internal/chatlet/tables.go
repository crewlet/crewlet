package chatlet

import "github.com/crewlet/crewlet/internal/statelog"

// THE TABLE INVENTORIES, and what each list is for.
//
// Three readers derive from these and none of them can see the others:
// [Domain.Tables], which the framework turns into a snapshot's scrub list, an
// identity claim and a local sweep; the completeness walk, which asserts that
// a replay from zero reproduces every one of them; and the applier, which is
// the only writer of any of them.
//
// THE NAMES ARE THE MIGRATION'S, EXACTLY. Nothing in Go connects a string here
// to `internal/store/schema/replicated/0055_the_chat_domain_lands.sql` (and
// `0068_a_chat_message_is_decided_where_it_is_applied.sql`, the verdicts,
// `0070_the_chat_verdicts_say_how_far_back_they_were_pruned.sql`, and
// `0071_a_room_says_what_changed_in_it.sql`, the auto-join account), and a
// misspelling is silent in the direction that matters: a table left out of the
// map is scrubbed from no snapshot, compared by no claim and swept by nobody,
// which looks exactly like a table that behaves. What catches it is the
// framework's own suite, which counts the rows of every table this map names.

// ReproducibleTables is every table a record's payload must be able to
// rebuild.
//
// THE COMPLETENESS WALK AS A LIST, so a table added later without a record to
// fill it goes red rather than shipping. It is what a replay from zero into an
// empty database has to end up with.
//
// THREE TABLES ARE DELIBERATELY ABSENT, and they are the log's own machinery
// rather than state a record reproduces: the operation ledger, the deferred
// records and their scope index. The framework's checkpoint is absent for the
// same reason and is not this domain's to name.
var ReproducibleTables = []string{
	// The room, its address and who is in it.
	//
	// `chat_channel_names` is one of them rather than machinery: a name
	// is an ADDRESS, and a node whose claims were not rebuilt would let a
	// second room take a name the first already holds.
	"chat_channels", "chat_channel_names", "chat_members",

	// The seats an auto-join room has accounted for ([Channel.AutoJoin]):
	// written by the patch that turns the setting on and by every
	// membership record that puts a newcomer in. A node whose replay left
	// it out would put every seat of the company in the room at its next
	// reconcile, the people who left it included.
	"chat_autojoin",

	// The transcript, and the four child tables DERIVED from it. Each
	// exists because a FILTER reads it — a collection inside a document
	// cannot be indexed, and a query that decodes every row's document is
	// a full scan wearing an index's name — and none of them is a claim a
	// record makes: who has spoken in a thread and who was named in a
	// message are facts about the messages themselves.
	"chat_messages", "chat_thread_participants", "chat_follows",
	"chat_reactions", "chat_mentions",

	// The compliance erase's marker, which is how a node tells a message
	// that never existed from one the company destroyed — and what stops a
	// redelivered post resurrecting it.
	"chat_deletions",

	// What the applier decided about every message record, which the
	// writer reads back before it answers and the change feed before it
	// wakes anybody ([Verdict]) — the record's own decision rather than
	// the log's machinery, so a node whose replay left it out would answer
	// a retry of a refused post with nothing and wake a room about it —
	// and how far back the horizon has pruned those decisions, without
	// which a pruned verdict reads as a record a gate dropped.
	"chat_verdicts", "chat_verdicts_lost",

	// And the framework's two per-domain tables: the eviction fence,
	// whose positions name this stream's own number space, and the
	// reanchor's audit row.
	"chat_evictions", "chat_log_generations",
}

// MachineryTables are the log's own, excluded from the walk and from the
// identity claim.
//
// THEY DO NOT ALL TRAVEL. The deferred record and its scope are this node's
// own verdict about bytes this build could not decode, and are scrubbed out of
// every donated snapshot. The operation ledger (chat_ops) TRAVELS: its rows are
// what every node's applier writes from the same records, so the donor's are
// exactly the ones this node would have written, and an adopter without them
// cannot tell a first attempt from a retry of anything the donor applied — see
// [statelog.Domain.OpsTable].
var MachineryTables = []string{
	"chat_ops", "chat_log_deferred", "chat_log_deferred_scope",
}

// BarrierTables is the empty set, DECLARED.
//
// The barrier writes no row on any node, and stating that explicitly is what
// stops a no-op record slipping through the kind-completeness walk by writing
// nothing: "this kind wrote nothing" and "nobody classified this kind" are the
// same observation from the walk's side, and only one of them is correct.
var BarrierTables = map[string]statelog.TableClass{}

// Tables is every durable table this domain writes, with its class.
//
// THE SCRUB LIST, THE IDENTITY CLAIM AND THE LOCAL SWEEP ARE ALL DERIVED FROM
// THIS MAP, which is why a table missing from it is three lists that are
// silently short rather than one error. It is built from the exported
// inventories rather than typed a third time, and it lives here beside them
// rather than with the rest of the declaration for exactly that reason.
//
// THREE CLASSES, AND THE ONE DIVERGENT TABLE IS THE LEDGER. No ROW table is
// Divergent, and that is a property of the applier rather than an omission: it
// reads no epoch key and no node identity, so every node writes identical rows
// and the identity claim covers the whole of the conversation. The wiki's
// tool-skill flag is the shape a Divergent row table takes — THIS BUILD'S
// parser answering about this build's rules, which two builds mid-upgrade
// legitimately disagree about — and nothing in a conversation is in that
// position. The operation ledger is Divergent for the framework's reason
// instead: it travels inside every snapshot, and its `applied_at` is each
// node's own clock. A table that ever joins it belongs in this function with
// the class stated, never in [ReproducibleTables] with a comment.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	// THE LEDGER TRAVELS — see [statelog.Domain.OpsTable] and
	// [MachineryTables].
	out[Domain{}.OpsTable()] = statelog.Divergent
	return out
}
