package chatlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write this node
// must not make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam over the log spec names — the one
// the publisher writes, whose checkpoint and anchors the seam reads.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, chatGuards)
}

// chatGuards answers the two object-level facts a first write needs.
//
// TWO KINDS HAVE A GUARDING ROW, and they are exactly the two a create ever
// takes a first-writer-wins append on ([statelog.PatternCreate]):
//
//   - AN ADDRESS. `chat_channel_names` is what still says a name is taken once
//     the create that took it has been trimmed off the log — which is the whole
//     reason a create publishes at an expectation of zero and the row is what
//     protects the retry-at-zero branch below the trim floor.
//   - A DIRECT CONVERSATION'S OWN ROOM. Its id is DERIVED from its
//     participants ([DirectChannelID]), so two sides opening it from two nodes
//     publish the SAME subject, and the room's own row is what tells the loser
//     that the conversation is already there.
//
// NOTHING HERE IS EVER DELETED, and that is a property of the domain rather
// than an omission: a room is ARCHIVED — closed to new messages with every
// word of it still readable — and the one destructive gesture,
// [MessageErase], removes MESSAGE rows. Its marker in `chat_deletions` is
// keyed on a message id, and a message is never a subject in this alphabet: a
// message record is published on its ROOM. So there is no subject a deletion
// marker could gate, which is why the applier does not install that gate
// either ([Applier.Gated]) and why the erase marker is consulted inside the
// apply instead ([messageErased]).
func chatGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	switch ObjectKind(subj.Kind) {
	case KindChannelName:
		var held int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_channel_names WHERE name_token = ?`,
			subj.ID).Scan(&held)
		if err != nil {
			return false, false, fmt.Errorf("chatlet: read the guard on the "+
				"address %s: %w", subj.ID, err)
		}
		return false, held > 0, nil
	case KindChannel:
		var present int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_channels WHERE id = ?`, subj.ID).
			Scan(&present)
		if err != nil {
			return false, false, fmt.Errorf("chatlet: read the guard on room "+
				"%s: %w", subj.ID, err)
		}
		return false, present > 0, nil
	}
	return false, false, nil
}

// ReadScope is the closure a READ is about.
//
// It is the same alphabet a record's own scope resolves into, which is what
// makes the coverage probe one comparison rather than a translation between
// two vocabularies. A read with no room named is the DOMAIN — not because it
// touches everything, but because a read that cannot say what it is about is
// one every deferred record concerns, and the honest answer to "is this
// complete" is then "no".
//
// THERE IS NO PER-MESSAGE READ SCOPE, for the reason there is no per-message
// TERM: a message's path is its room's, so a read of one message and a read of
// its whole transcript are about the same closure.
func ReadScope(channelID string) statelog.ScopeSet {
	if channelID != "" {
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermChannel, ID: channelID}.Path(),
		}}
	}
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermDomain}.Path()}}
}

// Fence refuses a write this node must not make.
//
// THE SAME TWO PRICES THE KNOWLEDGE BASE'S AND THE TRACKER'S PAY, because this
// domain installs the same gate: Evicted runs on every append and is answered
// from rows this node already has, and ClearForZero pays a coordination round
// trip because being wrong there is a second room holding one address rather
// than a duplicate.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor is the fleet's published trim floor at a generation and Ends
	// the log's two ends, read live — the first surviving sequence is the
	// other bound [Fence.ClearForZero] takes the higher of, for the reason
	// the tracker's fence gives, and the last is what says this node is on
	// this log at all. The cursor the floor is compared against is passed
	// per call, and the floor is read at that cursor's generation, because
	// the cursor has to be the checkpoint the write's own snapshot read
	// ([statelog.Snap.Checkpoint]) — see the tracker's fence for why
	// neither may be the applier's live position. All three are set by
	// the engine after the runner exists, because a fence built before its
	// applier would compare against a position that does not move.
	Floor func(ctx context.Context, generation uint32) (uint64, error)
	Ends  func(ctx context.Context) (statelog.LogEnds, error)

	// Committed is this node's applier's live checkpoint, which the end is
	// compared against instead of the cursor, for the reason
	// [statelog.ZeroFence] gives.
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS.
//
// Not from coordination, which is the point: a wedged coordination path is a
// precondition of an eviction being permitted at all, so the source that is
// still fresh in exactly that failure is this node's own replicated table.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, NOT A POOL. A replicated estate that is not open —
	// a legitimate, documented state, since an adoption closes it between
	// its rename and its reopen — has no pool, and a statement issued on a
	// closed one panics inside database/sql. [store.ReplicatedReader.Read]
	// answers [store.ErrNoEstate], which the refusal below already handles
	// as the honest "unreadable is not not-evicted".
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position
		FROM chat_evictions WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNREADABLE IS NOT "not evicted". A fence that failed open on a
		// store it could not read is a node appending records every
		// other node drops, collecting acknowledgements for messages
		// that reach nobody.
		return false, fmt.Errorf("chatlet: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is a deliberate departure
// from the fail-open rule a delivery claim uses: failing open there is a
// duplicate delivery, which is recoverable; failing open here is a second room
// holding one address, which is not.
//
// THE CHECK IS [statelog.ZeroFence], the one the tracker's and the knowledge
// base's fences run too: this supplies the four reads. Written out here it
// refused an evicted node with [statelog.ErrConflict], which [Store.publish]
// translates into "the room kept changing", compared the floor with the cursor
// alone — clearing a node whose checkpoint was past the log's end — and refused
// a node below the floor with prose rather than a reason; every refusal is now
// an [*statelog.Unavailable] naming it.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node.
//
// ONE GATE, AND IT IS THE ONE [Applier.Gated] INSTALLS. The two must agree: a
// resolution that looked for a gate the applier never installs would read
// every unapplied record as "somebody else won", and one that missed a gate
// the applier does install would re-decide and republish a record nothing
// applies until the round budget ran out.
//
// THE ERASE MARKER IS NOT ONE OF THEM, for the reason the applier states: a
// message record's subject is its ROOM, so the ids a marker is keyed on live
// in a payload — and a record about an erased message still APPLIES, writing
// its ledger row and nothing else, so the publisher resolves it from the
// ledger like any other. There is no deletion gate to report first, so this
// reader keeps the half of the rule [statelog.Gates] states that a domain
// without one has: the eviction window, half-open at both ends.
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports the gate that dropped a record at p — certified, with the
// applier it has to agree with, by statelogtest.RunEvictionGates.
//
// The record's body is not read, although it is handed one for its own
// operation: the one gate here is the writer's eviction window, which the
// envelope alone answers, and the erase marker a payload names is not a gate
// at all — a record about an erased message still applies, writing its ledger
// row and nothing else.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, _ []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() || writer == "" {
		return "", false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, NOT A POOL — see [Fence.Evicted] for why a closed
	// pool is reachable here.
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position
		FROM chat_evictions WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("chatlet: read the eviction gate for node "+
			"%s: %w", writer, err)
	}
	// THE APPLIER'S WINDOW, spelled as [Applier.Gated] spells it: the two
	// must agree, and the same two comparisons are what makes that readable
	// rather than derived.
	at := p.Packed()
	evicted := from.Valid && at > from.Int64
	back := readmitted.Valid && at >= readmitted.Int64
	if evicted && !back {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}
