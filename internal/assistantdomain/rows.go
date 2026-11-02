package assistantdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write it must not
// make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam over the log spec names.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, guards)
}

// guards answers the two object-level facts a first write needs: a deletion
// marker (permanent) and a guarding row (what still says an object exists once
// the record that created it has been trimmed off the log).
//
// THREE KINDS HAVE THEM: a conversation (its row and its marker) and a turn and
// a proposal (their rows and their conversation's marker). An owner's subject
// is never created, only arbitrated on, and the framework's kinds are created
// by their first record and removed by nothing.
func guards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (deleted, guard bool, err error) {
	var table string
	switch ObjectKind(subj.Kind) {
	case KindConversation:
		table = "assistant_conversations"
	case KindTurn:
		table = "assistant_turns"
	case KindProposal:
		table = "assistant_proposals"
	default:
		return false, false, nil
	}
	conversation, ok := gatedConversation(subj.Kind, subj.ID)
	if !ok {
		return false, false, fmt.Errorf("assistantdomain: %s names no conversation", subj)
	}
	id := conversation
	if ObjectKind(subj.Kind) != KindConversation {
		_, id, _ = splitChild(subj.ID)
	}
	var removed, present int
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM assistant_deletions WHERE conversation_id = ?),
		       (SELECT COUNT(*) FROM `+table+` WHERE id = ?)`,
		conversation, id).Scan(&removed, &present); err != nil {
		return false, false, fmt.Errorf("assistantdomain: read the guards on %s: %w", subj, err)
	}
	return removed > 0, present > 0, nil
}

// Fence refuses a write this node must not make: Evicted on every append, from
// rows this node already has, and ClearForZero before an expectation of zero,
// which pays a coordination round trip because being wrong there is a lost
// update.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor is the fleet's published trim floor at a generation, Ends the
	// log's two ends read live, and Committed this node's applier's live
	// checkpoint: the reads [statelog.ZeroFence] takes.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS, which
// are the one source still fresh when coordination is wedged. UNREADABLE IS NOT
// "not evicted": a fence that failed open would collect acknowledgements for
// writes every other node drops.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM assistant_evictions
			WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("assistantdomain: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of zero is
// safe from this node ([statelog.ZeroFence], the check every strict domain's
// fence runs).
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node: the gates
// [Applier.Gated] installs, read from the publisher's side by the rule
// [statelog.Gates] states.
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it: THE DELETION GATES FIRST (facts about the object, holding for
// every writer for ever), over the subjects the applier's own gate covers
// ([gatedConversation]), with the purge's own record exempt by its operation
// id, and the erasure gate where the record's own body names its owner; then
// the writer's eviction window. One transaction for every half, through the
// handle, so the answer describes one committed state.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() {
		return "", false, nil
	}
	var reason statelog.Reason
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		if conversation, ok := gatedConversation(subj.Kind, subj.ID); ok {
			author, marked, err := deletionMarker(ctx, tx, conversation)
			if err != nil {
				return err
			}
			if marked && author != opID {
				reason = statelog.ReasonDeleted
				return nil
			}
		}
		var op string
		if len(body) > 0 {
			if env, err := DecodeEnvelope(body); err == nil {
				op = string(env.Op)
			}
		}
		erased, err := erasedOwnerOf(ctx, tx, subj.Kind, op, body)
		if err != nil {
			return err
		}
		if erased {
			reason = statelog.ReasonDeleted
			return nil
		}
		if writer == "" {
			return nil
		}
		evicted, err := evictedAt(ctx, tx, writer, p.Packed())
		if err != nil {
			return err
		}
		if evicted {
			reason = statelog.ReasonEvicted
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, reason != "", nil
}
