package chatlet

import (
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LIVE SEAM: how a committed record reaches a screen that is already open.
//
// The applier is what a socket listens to, not the change feed: a wake must
// outlive a writer dying before the publish, but a frame is worthless to a
// viewer not looking, so a durable consumer would retain frames for closed
// screens. The applier says what it wrote AFTER it is durable, and a missed
// frame refetches exactly: holding 41 and handed 43, a browser asks for 42.
//
// It accumulates rather than calls: [Applier.Apply] runs INSIDE an optimistic
// transaction whose conflicted body re-runs, so effects are gathered there and
// drained in [Applier.Committed], called once, after the commit.

// Observer is what a live surface implements to hear committed chat.
//
// ONE METHOD TAKING THE WHOLE BATCH: a transaction is what became durable, and
// record-by-record fan-out would render a half-applied batch. The
// implementation belongs to the API layer; this package knows nothing of
// sockets, viewers or visibility.
//
// IT MUST NOT BLOCK: it runs on the applier's own goroutine between one batch's
// commit and the next's transaction, so a slow observer slows the log on this
// node. Hand the batch to a buffer and return.
type Observer interface {
	Applied(batch []Applied)
}

// Applied is one record's committed effect, as a live surface needs it.
//
// A VALUE, NOT THE RECORD: a subscriber needs which room, what happened, who,
// and for a message which one at which sequence, to tell a frame it can apply
// from a hole it must refetch. The [MutationRecord] would promote the whole
// record format to a UI contract nothing could then change. The position is
// here because a viewer's cursor is a position.
//
// IT CROSSES THE FLEET, so every field is named on the wire: a node whose own
// copy is out of service is relayed a sound peer's batches (the engine's live
// relay), and a field lost in the trip renders a frame without it on the node
// nobody vouches for. Additive, like every value crossing between builds.
type Applied struct {
	// Position is where the record that caused this sits on the log.
	Position statelog.Position `json:"position"`

	// Subject and Op are what the record was: the room's own state, its
	// address, or a message in it.
	Subject Subject `json:"subject"`
	Op      OpKind  `json:"op"`

	// OpID is the record's idempotency key, so a caller that published
	// this gesture recognises its own effect coming back.
	OpID string `json:"op_id"`

	// ChannelID is the room, always — including for a message, whose
	// subject id IS the room's.
	ChannelID string `json:"channel_id"`

	// MessageID and ChannelSeq are the message this record wrote, and the
	// contiguous number it landed at. Both empty and zero for a record
	// that was not about one message.
	MessageID  string `json:"message_id,omitempty"`
	ChannelSeq int64  `json:"channel_seq,omitempty"`

	Actor     string     `json:"actor"`
	ActorKind AuthorKind `json:"actor_kind"`

	// At is the BROKER'S own instant for the record, which is what every
	// node renders and therefore what a live frame carries. A local clock
	// here would make one viewer's timeline disagree with another's.
	At time.Time `json:"at"`

	// Notify is the routing snapshot the record carried, nil when it woke
	// nobody. It is passed through rather than interpreted: whether a
	// viewer is in the recipient set is a question about that viewer, and
	// this package has no idea who is watching.
	Notify *Notify `json:"notify,omitempty"`

	// Removed is who a membership record took OUT of the room, sorted, and
	// empty for every other record.
	//
	// A LIVE SURFACE CANNOT DERIVE IT, which is why the applier carries it.
	// A frame is filtered by whether its viewer may read the room at the
	// batch's own position, and at that position the people this record
	// removed may not — so they were the one audience a membership change
	// never reached, and a private room stayed in their rail, openable and
	// unread-badged, until something made it read again. They are told the
	// room changed and nothing else: [Applied] carries no member list, and
	// what they already knew is that they were in it.
	Removed []string `json:"removed,omitempty"`
}

// accumulator gathers a transaction's effects and drains them once.
//
// A map keyed on the packed position, because the transaction's body RE-RUNS
// with the same records: an appended slice would carry a contended room's
// message twice, rendered as the remark said twice. A position is unique per
// record in a log, so a re-run overwrites its own earlier entry.
//
// NOT GUARDED BY A MUTEX, on the framework's contract: an applier is ONE
// writer, and Apply and Committed run on one goroutine with the commit between.
type accumulator struct {
	obs Observer
	at  map[int64]Applied
}

// newAccumulator builds the live half for one applier. A NIL OBSERVER IS
// LEGAL and is a no-op, which is what a test that renders no room runs. A
// running node always hands one — the engine's own hook, which keeps every
// batch for the peers it relays to whether or not a dashboard is attached
// here.
func newAccumulator(obs Observer) *accumulator {
	return &accumulator{obs: obs}
}

// note records one committed effect, to be drained after the commit.
//
// IT IS CALLED ONLY WHERE A ROW ACTUALLY MOVED. A record the version guard
// skipped wrote nothing, and a frame for it would tell a screen that something
// changed when the change was somebody else's, already rendered.
func (a *accumulator) note(applied Applied) {
	if a == nil || a.obs == nil {
		// NOTHING IS GATHERED WHEN NOBODY IS LISTENING, rather than
		// gathered and dropped: this runs once per applied record on
		// the busiest log in the engine, and a map that only ever grew
		// until the next commit would be a cost paid for a screen that
		// does not exist.
		return
	}
	if a.at == nil {
		a.at = make(map[int64]Applied, 8)
	}
	a.at[applied.Position.Packed()] = applied
}

// discard throws away everything gathered so far.
//
// CALLED WHEN THE APPLY RETURNS AN ERROR, because [Applier.Committed] runs
// only on success: entries from a batch that never committed would otherwise
// sit in the map and drain on a LATER commit, announcing rows that were never
// written. It clears the whole map rather than the failing record's own entry,
// because the transaction is what failed and every record in it is rolled
// back with it.
func (a *accumulator) discard() {
	if a == nil {
		return
	}
	clear(a.at)
}

// drain hands the batch to the observer and empties the accumulator.
//
// THE BATCH IS IN POSITION ORDER, which is the log's own: the map is keyed on
// the packed position precisely so a re-run cannot duplicate an entry, and a
// map has no order at all — so a frame stream built from one unsorted would
// render a reply before the message it answers.
func (a *accumulator) drain() {
	if a == nil || a.obs == nil || len(a.at) == 0 {
		return
	}
	batch := make([]Applied, 0, len(a.at))
	for _, packed := range slices.Sorted(maps.Keys(a.at)) {
		batch = append(batch, a.at[packed])
	}
	clear(a.at)
	a.obs.Applied(batch)
}
