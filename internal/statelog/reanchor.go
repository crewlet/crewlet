package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// ErrReanchorRefused reports a generation transition that was not permitted.
var ErrReanchorRefused = errors.New("statelog: reanchor refused")

// ReanchorGuard is what an operator must satisfy before the estate's
// generation moves.
type ReanchorGuard struct {
	// Confirm is what the operator typed, which must match the live
	// stream's own creation instant. It is a guard against running the
	// verb on the wrong estate — the one mistake that cannot be undone.
	Confirm string

	// Force overrides the "only the most caught-up node may reanchor"
	// rule, and its message must name exactly what may be lost.
	Force bool
}

// ReanchorInputs is everything the guards read.
type ReanchorInputs struct {
	// StreamCreatedAt is the LIVE stream's creation instant, which is what
	// the operator's confirmation is checked against.
	StreamCreatedAt time.Time

	// FirstSeq is the live stream's first surviving sequence. The new
	// cursor is one below it, which is zero on a fresh stream.
	FirstSeq uint64

	// PeersHydrated is how many peers are caught up on the LIVE stream.
	//
	// ANY IS A REFUSAL, and the reason is not caution: two nodes that
	// reanchor independently each keep whatever they applied off the old
	// stream before it vanished, and if those prefixes differ the identity
	// claim is silently violated for ever — with no log left to reconcile
	// them from.
	PeersHydrated int

	// Position is this node's own committed sequence at the OLD
	// generation, and Highest is the highest any counted node published.
	// Only the most caught-up node may reanchor when nobody is hydrated,
	// because whatever it did not apply is what the fleet loses.
	Position uint64
	Highest  uint64

	// RegisterReadable reports whether the register could be listed at
	// all. When it could not, the comparison above cannot be made and only
	// an explicit force proceeds.
	RegisterReadable bool

	// Generation is the highest generation this estate has recorded, read
	// LOCALLY from the domain's own audit table — no coordination read,
	// because the verb runs when coordination may be exactly what was
	// lost.
	Generation uint32
}

// PermitReanchor decides whether the transition may run, and to which
// generation.
func PermitReanchor(in ReanchorInputs, guard ReanchorGuard) (uint32, error) {
	if guard.Confirm == "" {
		return 0, fmt.Errorf("%w: confirm the live stream's creation instant "+
			"(%s) — this verb rewrites every object's arbitration anchor and "+
			"there is no undo", ErrReanchorRefused,
			in.StreamCreatedAt.UTC().Format(time.RFC3339))
	}
	if guard.Confirm != in.StreamCreatedAt.UTC().Format(time.RFC3339) {
		return 0, fmt.Errorf("%w: the confirmation names %q and the live stream "+
			"was created at %s — running this on the wrong estate cannot be "+
			"undone", ErrReanchorRefused, guard.Confirm,
			in.StreamCreatedAt.UTC().Format(time.RFC3339))
	}
	if in.PeersHydrated > 0 {
		return 0, fmt.Errorf("%w: %d peer(s) are hydrated on the live stream. "+
			"Two nodes that reanchor independently each keep whatever they "+
			"applied off the old stream before it vanished, and if those "+
			"prefixes differ the identity claim is violated silently and for "+
			"ever, with no log left to reconcile them from — adopt a hydrated "+
			"peer's snapshot instead", ErrReanchorRefused, in.PeersHydrated)
	}
	if !guard.Force {
		if !in.RegisterReadable {
			return 0, fmt.Errorf("%w: the positions register could not be read, "+
				"so whether this is the most caught-up node is unknown — and "+
				"whatever it did not apply is what the fleet loses. Re-run with "+
				"the force flag if that is accepted", ErrReanchorRefused)
		}
		if in.Position < in.Highest {
			return 0, fmt.Errorf("%w: this node is at %d and the fleet reached "+
				"%d — only the most caught-up node may reanchor, because "+
				"everything above its own position is what the reanchor "+
				"discards", ErrReanchorRefused, in.Position, in.Highest)
		}
	}
	// THE NEW GENERATION IS DERIVED LOCALLY, from the estate's own audit
	// table: this verb runs when the broker estate has been lost, so a
	// generation that needed a coordination read could not be computed at
	// the one moment it is needed.
	return in.Generation + 1, nil
}

// ReanchorDeps is everything the transition needs that it does not own.
type ReanchorDeps struct {
	// Domains are every domain whose cursor moves, by stream name.
	Domains map[string]Registered

	// DB is the replicated estate, which holds every cursor.
	DB *store.DB

	// ResetVersions rewrites every object's arbitration anchor into the
	// new generation, in BOUNDED, RESUMABLE transactions.
	//
	// Bounded because half a million rows cannot be one transaction and
	// must not hold this store's only writer for the minutes it would
	// take. Resumable because it is correct for it to be interrupted: a
	// row the reset did not reach carries a generation BELOW the current
	// one, which is the "no anchor at this generation" branch — so its
	// next write forms an expectation of zero and the broker arbitrates it
	// against a subject that genuinely holds nothing.
	ResetVersions func(ctx context.Context, gen uint32) error

	// PublishGeneration appends the one record that makes the transition
	// fleet-visible, first-writer-wins on its own subject. A second node
	// racing it LOSES and reads the winner's record on replay.
	PublishGeneration func(ctx context.Context, gen uint32, in ReanchorInputs) error

	// RecordGeneration writes the audit row, in the SAME transaction as
	// the cursors.
	RecordGeneration func(ctx context.Context, tx *sql.Tx, gen uint32, in ReanchorInputs) error

	Logger *slog.Logger
	Now    func() time.Time
}

// Reanchor runs the generation transition.
//
// # Seven steps, and the order is its crash matrix
//
//  1. Read the live stream and check the operator's confirmation.
//  2. Refuse while any peer is hydrated, and — when none is — require this to
//     be the most caught-up node.
//  3. Derive the new generation LOCALLY.
//  4. Reset every object's anchor, in BOUNDED transactions. A crash here
//     leaves a partly-reset table and NEEDS NO REPAIRER: every row the reset
//     did not reach is covered by the lazy rule, and re-running finishes it.
//  5. Publish ONE record on the generation's own subject, at an expectation of
//     zero. A crash after the append leaves the record on the stream: the
//     re-run derives the SAME generation, races itself, loses, and reads the
//     winner's record on replay — first-writer-wins used for the one thing it
//     is perfectly suited to.
//  6. and 7. ONE transaction: every domain's cursor into the new generation,
//     and the audit row. A crash rolls both back whole.
//
// The bounded step 4 and the single transaction at 6+7 are two different
// designs with incompatible crash matrices, and only the bounded one is
// correct — which is true ONLY because the lazy rule makes a partial reset
// harmless. The two are stated together for that reason.
func Reanchor(ctx context.Context, d ReanchorDeps, in ReanchorInputs, guard ReanchorGuard) (uint32, error) {
	switch {
	case len(d.Domains) == 0:
		return 0, fmt.Errorf("statelog: a reanchor with no registered domain " +
			"moves no cursor")
	case d.DB == nil:
		return 0, fmt.Errorf("statelog: a reanchor has no store")
	case d.ResetVersions == nil || d.PublishGeneration == nil || d.RecordGeneration == nil:
		return 0, fmt.Errorf("statelog: a reanchor is missing one of its three " +
			"steps, and a partial one leaves the fleet unable to write")
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}

	gen, err := PermitReanchor(in, guard)
	if err != nil {
		return 0, err
	}
	logger.WarnContext(ctx, "statelog_reanchor_started",
		"generation", gen, "stream_created_at", in.StreamCreatedAt,
		"position", in.Position, "forced", guard.Force)

	// 4. THE RESET, bounded and resumable.
	if err := d.ResetVersions(ctx, gen); err != nil {
		return 0, fmt.Errorf("statelog: reset the anchors into generation %d: %w",
			gen, err)
	}

	// 5. THE RECORD, first-writer-wins. A crash after it is the one
	// residue with a repairer that is already in the design.
	if err := d.PublishGeneration(ctx, gen, in); err != nil {
		return 0, fmt.Errorf("statelog: publish generation %d: %w", gen, err)
	}

	// 6+7. ONE transaction, both cursors and the audit row.
	//
	// The new cursor is ONE BELOW the live stream's first surviving
	// sequence — zero on a fresh stream — because that is the position
	// from which everything the stream still holds is un-applied.
	cursor := uint64(0)
	if in.FirstSeq > 0 {
		cursor = in.FirstSeq - 1
	}
	if err := d.DB.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		for _, reg := range d.Domains {
			// newTables rather than a literal: a hand-built one leaves
			// whatever field the writer did not think of at its zero
			// value, and every one of them is a key some other side of
			// the framework spells in full.
			t, err := newTables(reg.Domain)
			if err != nil {
				return err
			}
			at := Position{Stream: t.stream, Generation: gen, Seq: cursor}
			if err := t.setCursor(ctx, tx, at, in.StreamCreatedAt, now()); err != nil {
				return err
			}
		}
		return d.RecordGeneration(ctx, tx, gen, in)
	}); err != nil {
		return 0, fmt.Errorf("statelog: move the cursors into generation %d: %w",
			gen, err)
	}

	logger.WarnContext(ctx, "statelog_reanchored",
		"generation", gen, "cursor", cursor, "domains", len(d.Domains))
	return gen, nil
}
