package chat

import (
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE LIVE SEAM: how a committed record reaches a screen that is already open.
//
// # Why the applier is what a socket listens to, and why it is not a wake
//
// A wake is derived by something that OUTLIVES the writer — the change feed,
// over the committed record — because a wake runs a turn and must survive the
// node that wrote the record dying between the commit and the publish. That
// argument does not carry to a BROWSER. A socket frame is worthless to a
// viewer who is not looking, has no retry that could help after the fact, and
// is wanted within milliseconds by whoever is looking now; a durable consumer
// for it would be a second delivery path per node per viewer, retaining frames
// for screens that closed.
//
// So the live path is the cheapest thing that is still honest: the applier
// says what it wrote, AFTER it is durable, and anything that missed a frame
// refetches. The transcript's contiguous per-channel sequence is what makes
// that refetch exact — a browser holding 41 and handed 43 asks for 42 rather
// than for the room.
//
// # Why it accumulates rather than calls
//
// [Applier.Apply] runs INSIDE the transaction, and the store's transactions
// are optimistic: a conflicted one re-runs its body, so anything called from
// there happens more than once for one record. Everything published here is
// therefore gathered in Apply and drained in [Applier.Committed], which the
// framework calls once, after the commit.

// Observer is what a live surface implements to hear committed chat.
//
// ONE METHOD TAKING THE WHOLE BATCH, rather than one call per record: a
// transaction is what became durable, and a subscriber that fanned out record
// by record would render a half-applied batch on the way through. The
// implementation belongs to the API layer — this package knows nothing about
// sockets, viewers or visibility, and a chat applier that did would be one
// that could not be tested without one.
//
// IT MUST NOT BLOCK. It is called on the applier's own goroutine, between one
// batch's commit and the next batch's transaction, so a slow observer is a log
// that applies slowly on this node. An implementation that fans out to
// sockets hands the batch to its own buffer and returns.
type Observer interface {
	Applied(batch []Applied)
}

// Applied is one record's committed effect, as a live surface needs it.
//
// # Why it is a value and not the record
//
// A subscriber renders a room. What it needs is which room, what happened,
// who did it and — for a message — which message at which sequence, so it can
// tell a frame it can apply from a hole it must refetch. Handing it the
// [MutationRecord] instead would hand every subscriber the scope alphabet,
// the version gate and the typed payloads, which is the whole record format
// promoted to a UI contract that nothing could then change.
//
// THE POSITION IS HERE because a viewer's cursor is a position: a screen that
// reconnects says where it had reached, and a frame it cannot place is a frame
// it must throw away.
type Applied struct {
	// Position is where the record that caused this sits on the log.
	Position statelog.Position

	// Subject and Op are what the record was: the room's own state, its
	// address, or a message in it.
	Subject Subject
	Op      OpKind

	// OpID is the record's idempotency key, so a caller that published
	// this gesture recognises its own effect coming back.
	OpID string

	// ChannelID is the room, always — including for a message, whose
	// subject id IS the room's.
	ChannelID string

	// MessageID and ChannelSeq are the message this record wrote, and the
	// contiguous number it landed at. Both empty and zero for a record
	// that was not about one message.
	MessageID  string
	ChannelSeq int64

	Actor     string
	ActorKind AuthorKind

	// At is the BROKER'S own instant for the record, which is what every
	// node renders and therefore what a live frame carries. A local clock
	// here would make one viewer's timeline disagree with another's.
	At time.Time

	// Notify is the routing snapshot the record carried, nil when it woke
	// nobody. It is passed through rather than interpreted: whether a
	// viewer is in the recipient set is a question about that viewer, and
	// this package has no idea who is watching.
	Notify *Notify
}

// accumulator gathers a transaction's effects and drains them once.
//
// # Why it is a map keyed on the packed position
//
// Because the transaction's body RE-RUNS. The store's transactions are
// optimistic, so a conflicted one is replayed from the top with the same
// records in the same order — and an appended slice would then carry a
// contended room's message twice, which every viewer renders as the same
// remark said twice. A position is unique per record within a log, so a re-run
// overwrites its own earlier entry and the batch is the same either way.
//
// NOT GUARDED BY A MUTEX, on the framework's own contract: an applier is ONE
// writer, and Apply and Committed are called from the same goroutine with the
// commit in between.
type accumulator struct {
	obs Observer
	at  map[int64]Applied
}

// newAccumulator builds the live half for one applier. A NIL OBSERVER IS
// LEGAL and is a no-op, which is what every test and every node with no
// dashboard attached runs.
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
		// until the next commit would be a cost every node pays for a
		// screen no node has open.
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
