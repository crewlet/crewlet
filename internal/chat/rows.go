package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write this node
// must not make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam.
func NewRows(db *store.DB) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, chatGuards)
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
			return false, false, fmt.Errorf("chat: read the guard on the "+
				"address %s: %w", subj.ID, err)
		}
		return false, held > 0, nil
	case KindChannel:
		var present int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_channels WHERE id = ?`, subj.ID).
			Scan(&present)
		if err != nil {
			return false, false, fmt.Errorf("chat: read the guard on room "+
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
// THE SAME TWO PRICES THE WIKI'S AND THE TRACKER'S PAY, because this domain
// installs the same gate: Evicted runs on every append and is answered from
// rows this node already has, and ClearForZero pays a coordination round trip
// because being wrong there is a lost update rather than a duplicate.
type Fence struct {
	db     *store.DB
	nodeID string

	// Cursor is this node's committed position, and Floor the published
	// trim floor. Both are set by the engine after the runner exists,
	// because a fence built before its applier would compare against a
	// position that does not move.
	Cursor func() statelog.Position
	Floor  func(ctx context.Context) (uint64, error)
}

// NewFence builds it.
func NewFence(db *store.DB, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS.
//
// Not from coordination, which is the point: a wedged coordination path is a
// precondition of an eviction being permitted at all, so the source that is
// still fresh in exactly that failure is this node's own replicated table.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db == nil || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, NOT ITS POOL. `DB.SQL()` answers a NIL pool on a
	// replicated estate that is not open — a legitimate, documented state
	// of that peer, since an adoption closes it between its rename and its
	// reopen — and a statement issued on it panics inside database/sql.
	// [store.DB.Read] answers [store.ErrNoEstate], which the refusal below
	// already handles as the honest "unreadable is not not-evicted".
	err := f.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
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
		return false, fmt.Errorf("chat: read this node's own eviction: %w", err)
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
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	evicted, err := f.Evicted(ctx)
	if err != nil {
		return err
	}
	if evicted {
		return fmt.Errorf("chat: this node is evicted, so a write at an "+
			"expectation of zero would be dropped by every peer: %w",
			statelog.ErrConflict)
	}
	if f.Floor == nil {
		return fmt.Errorf("chat: no published trim floor is readable, so this " +
			"node cannot establish that an absent anchor means an unclaimed " +
			"address rather than a claim trimmed beneath it")
	}
	floor, err := f.Floor(ctx)
	if err != nil {
		return fmt.Errorf("chat: read the published trim floor: %w — a floor "+
			"that cannot be read is not a floor that is low, and publishing at "+
			"zero on the guess is a second room with the same name", err)
	}
	// THE FLOOR IS THE FIRST SEQUENCE THE TRIM HAS NOT LICENSED REMOVING,
	// so a node that has consumed through the one before it has consumed
	// everything that may be gone — see the tracker's and the wiki's
	// fences, which make the same comparison for the same reason.
	if floor > cursor.Seq+1 {
		return fmt.Errorf("chat: the trim may have removed everything below %d "+
			"and this node has consumed through %d, so an absent anchor may be "+
			"an address claimed beneath it rather than one that was never "+
			"taken: %w", floor, cursor.Seq, statelog.ErrUnavailable)
	}
	return nil
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
// in a payload — and a gate that decoded the payload would answer "not gated"
// for exactly the records a rolling upgrade cannot read.
type Gates struct{ db *store.DB }

// NewGates builds it.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// GatedAt reports the gate that dropped a record at p.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil || writer == "" {
		return "", false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, NOT ITS POOL — see [Fence.Evicted] for why a nil
	// pool is reachable here.
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position
		FROM chat_evictions WHERE node_id = ?`, writer).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("chat: read the eviction gate for node "+
			"%s: %w", writer, err)
	}
	at := p.Packed()
	evicted := from.Valid && at > from.Int64
	back := readmitted.Valid && at >= readmitted.Int64
	if evicted && !back {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}

// AdoptedAt is when this node's adoption of a donated snapshot completed.
//
// It qualifies a read of the OPERATION LEDGER, which travels SCRUBBED inside a
// snapshot: an op id minted before this instant cannot be answered for here at
// all, and reading its absence as "somebody else won" would post a message
// that already landed a second time.
func (g *Gates) AdoptedAt(ctx context.Context) (time.Time, bool, error) {
	if g == nil || g.db == nil {
		return time.Time{}, false, nil
	}
	return statelog.AdoptedAt(ctx, g.db)
}
