package chat

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
// to `internal/store/schema/replicated/0015_the_chat_domain_lands.sql`, and a
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

	// The transcript, and the four child tables DERIVED from it. Each
	// exists because a FILTER reads it — a collection inside a document
	// cannot be indexed, and a query that decodes every row's document is
	// a full scan wearing an index's name — and none of them is a claim a
	// record makes: who has spoken in a thread and who was named in a
	// message are facts about the messages themselves.
	"chat_messages", "chat_thread_participants", "chat_follows",
	"chat_reactions", "chat_mentions",

	// The history, which is what an activity feed and a digest render
	// from.
	"chat_history",

	// The compliance erase's marker, which is how a node tells a message
	// that never existed from one the company destroyed — and what stops a
	// redelivered post resurrecting it.
	"chat_deletions",

	// And the framework's two per-domain tables: the eviction fence,
	// whose positions name this stream's own number space, and the
	// reanchor's audit row.
	"chat_evictions", "chat_log_generations",
}

// MachineryTables are the log's own, excluded from the walk and from the
// identity claim, and scrubbed out of every donated snapshot.
//
// A DONOR'S OPERATION LEDGER IS THE SHARPEST OF THEM: an adopted peer's ops
// table would let this node resolve its own ambiguous publish against somebody
// else's history, which is a write reported as landed that never happened.
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
// TWO CLASSES, AND THERE IS NO DIVERGENT TABLE AT ALL. That is a property of
// the applier rather than an omission: it reads no epoch key and no node
// identity, so every node writes identical rows and the identity claim covers
// the whole of what this domain holds. The wiki's tool-skill flag is the shape
// a Divergent table takes — THIS BUILD'S parser answering about this build's
// rules, which two builds mid-upgrade legitimately disagree about — and
// nothing in a conversation is in that position. A table that ever is belongs
// in this function with the class stated, never in [ReproducibleTables] with a
// comment.
func (Domain) Tables() map[string]statelog.TableClass {
	out := make(map[string]statelog.TableClass,
		len(ReproducibleTables)+len(MachineryTables))
	for _, table := range ReproducibleTables {
		out[table] = statelog.Replicated
	}
	for _, table := range MachineryTables {
		out[table] = statelog.Local
	}
	return out
}
