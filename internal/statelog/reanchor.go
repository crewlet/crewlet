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

	// Force overrides the two refusals that rest on the positions
	// register — a register that could not be read, and a node that is not
	// the most caught-up one — accepting the loss of every record the fleet
	// applied above this node's own position. It never overrides a hydrated
	// peer: two nodes re-anchored independently diverge silently, with no
	// log left to reconcile them from.
	Force bool
}

// ReanchorInputs is everything the guards read.
type ReanchorInputs struct {
	// StreamCreatedAt is the LIVE stream's creation instant, as the broker
	// reports it now: what the operator's confirmation is checked against,
	// and the instant the moved checkpoint is committed under — so the
	// applier that resumes from it, at this boot or the next, finds the
	// checkpoint on the stream it is running on. Zero when the broker could
	// not be asked, which [PermitReanchor] refuses.
	StreamCreatedAt time.Time

	// PrevStreamCreatedAt is the instant this node's checkpoint was
	// committed under — the stream the transition walks away from — and
	// zero for a node that holds none. Provenance for the audit row: the old
	// stream is gone, and its identity is recorded nowhere else.
	PrevStreamCreatedAt time.Time

	// FirstSeq is the live stream's first surviving sequence. The new
	// cursor is one below it, which is zero on a fresh stream.
	FirstSeq uint64

	// PeersHydrated is how many peers have applied records off the LIVE
	// stream.
	//
	// ANY IS A REFUSAL, and the reason is not caution: two nodes that
	// reanchor independently each keep whatever they applied off the old
	// stream before it vanished, and if those prefixes differ the identity
	// claim is silently violated for ever — with no log left to reconcile
	// them from.
	PeersHydrated int

	// Position is this node's own committed sequence at the OLD
	// generation, and Highest is the highest any node on that same stream
	// published. Only the most caught-up node may reanchor when nobody is
	// hydrated, because whatever it did not apply is what the fleet loses.
	Position uint64
	Highest  uint64

	// RegisterReadable reports whether the register could be listed at
	// all. When it could not, the comparison above cannot be made and only
	// an explicit force proceeds.
	RegisterReadable bool

	// Generation is the generation this node's checkpoint on the domain's
	// log stands at, read LOCALLY — no coordination read, because the verb
	// runs when coordination may be exactly what was lost.
	Generation uint32
}

// ConfirmResolution is how finely an operator's confirmation is compared with
// the live stream's creation instant.
//
// A SECOND, because the confirmation is a person's echo of an instant they
// read, and the two forms they are shown — RFC 3339 with the broker's
// fractional digits, as the status verb prints it, and without them — name the
// same second.
const ConfirmResolution = time.Second

// PermitReanchor decides whether the transition may run, and to which
// generation.
func PermitReanchor(in ReanchorInputs, guard ReanchorGuard) (uint32, error) {
	if in.StreamCreatedAt.IsZero() {
		// NOTHING TO CONFIRM AGAINST. A zero instant is a broker that did
		// not answer, and a transition committed under it records a
		// checkpoint no stream matches — the applier resuming from it
		// stops again, naming the recreation this verb was run to clear.
		return 0, fmt.Errorf("%w: the live stream's creation instant could not "+
			"be read from the broker, so there is nothing to confirm against "+
			"and nothing to commit the moved checkpoint under — retry once the "+
			"broker answers", ErrReanchorRefused)
	}
	live := in.StreamCreatedAt.UTC().Truncate(ConfirmResolution)
	if guard.Confirm == "" {
		return 0, fmt.Errorf("%w: confirm the live stream's creation instant "+
			"(%s) — this verb rewrites every object's arbitration anchor and "+
			"there is no undo", ErrReanchorRefused, live.Format(time.RFC3339))
	}
	confirmed, err := time.Parse(time.RFC3339Nano, guard.Confirm)
	if err != nil || !confirmed.UTC().Truncate(ConfirmResolution).Equal(live) {
		return 0, fmt.Errorf("%w: the confirmation names %q and the live stream "+
			"was created at %s — running this on the wrong estate cannot be "+
			"undone", ErrReanchorRefused, guard.Confirm, live.Format(time.RFC3339))
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
	// THE NEW GENERATION IS DERIVED LOCALLY, from this node's own
	// checkpoint: this verb runs when the broker estate has been lost, so a
	// generation that needed a coordination read could not be computed at
	// the one moment it is needed.
	return in.Generation + 1, nil
}

// GenerationOpID is the operation a node's generation record publishes under:
// the generation, the stream it re-anchors onto, and the node — the last
// because the record is a claim ([Publisher.Claim]), told from a rival's only
// by its op id.
//
// SHARED BETWEEN BUILDS like every op id, and read back only by the node that
// wrote it, when it re-runs a transition that crashed after its record landed.
// A record written under any other spelling reads to that re-run as a rival's.
func GenerationOpID(gen uint32, stream time.Time, nodeID string) string {
	return fmt.Sprintf("reanchor:%d:%d:%s", gen, stream.UTC().UnixNano(), nodeID)
}

// ReanchorDeps is everything the transition needs that it does not own.
type ReanchorDeps struct {
	// Domain is the ONE domain whose stream was recreated, and the only
	// one whose checkpoint moves.
	//
	// ONE, because a recreation is a fact about one stream. Every other
	// domain's checkpoint names a stream that was never touched, and moving
	// it to this stream's numbers and instant would stop that domain's
	// applier at its next boot, naming a recreation that never happened.
	Domain Domain

	// DB is the replicated estate, which holds every cursor.
	DB *store.DB

	// ResetVersions rewrites the domain's object rows' versions into the
	// new generation, in BOUNDED, RESUMABLE transactions. Nil for a domain
	// with nothing to reset.
	//
	// Bounded because half a million rows cannot be one transaction and
	// must not hold this store's only writer for the minutes it would
	// take. Resumable because a row the reset did not reach is still
	// written correctly: the expectation a write forms comes from its
	// subject's anchor, and an anchor below the current generation takes
	// the "no anchor at this generation" branch whether or not the row
	// beside it was reset.
	ResetVersions func(ctx context.Context, gen uint32) error

	// PublishGeneration appends the one record that makes the transition
	// fleet-visible, on the domain's NEW stream, as a claim on its own
	// subject ([Publisher.Claim]). REQUIRED for a domain that claims
	// identity: a second node racing the transition meets the first one's
	// record and is refused rather than moving a checkpoint of its own, and
	// two nodes re-anchored independently would each keep their own history
	// under one claim.
	PublishGeneration func(ctx context.Context, gen uint32, in ReanchorInputs) error

	// RecordGeneration writes the audit row, in the SAME transaction as the
	// cursor. Nil for a domain whose generation record's own apply is what
	// writes its audit row.
	RecordGeneration func(ctx context.Context, tx *sql.Tx, gen uint32, in ReanchorInputs) error

	Logger *slog.Logger
	Now    func() time.Time
}

// Reanchor runs the generation transition for one domain.
//
// # Seven steps, and the order is its crash matrix
//
//  1. Read the live stream and check the operator's confirmation.
//  2. Refuse while any peer is hydrated, and — when none is — require this to
//     be the most caught-up node.
//  3. Derive the new generation LOCALLY.
//  4. Reset the domain's versions, in BOUNDED transactions. A crash here
//     leaves a partly-reset table and NEEDS NO REPAIRER: every row the reset
//     did not reach is covered by the lazy rule, and re-running finishes it.
//  5. Publish ONE record on the generation's own subject, as a CLAIM
//     ([Publisher.Claim]) under an op id that names this node
//     ([GenerationOpID]). A crash after the append leaves the record on the
//     stream: the re-run derives the SAME generation and op id, finds its own
//     record holding the subject, and goes on to step 6 with nothing
//     appended. A second node that derived the same number finds a record it
//     did not write and is refused — [ErrReanchorRefused], carrying the
//     [ClaimedElsewhere] that names the holder — before any checkpoint of its
//     own moves.
//  6. and 7. ONE transaction: the domain's cursor into the new generation, and
//     the audit row. A crash rolls both back whole.
//
// The bounded step 4 and the single transaction at 6+7 are two different
// designs with incompatible crash matrices, and only the bounded one is
// correct — which is true ONLY because the lazy rule makes a partial reset
// harmless. The two are stated together for that reason.
//
// THE CALLER STOPS THE DOMAIN'S APPLIER FIRST and starts it again after: a
// running loop commits its own checkpoint after every batch, over the one step
// 6 writes, and only a loop started again reads the moved checkpoint.
func Reanchor(ctx context.Context, d ReanchorDeps, in ReanchorInputs, guard ReanchorGuard) (uint32, error) {
	switch {
	case d.Domain == nil:
		return 0, fmt.Errorf("statelog: a reanchor names no domain, so it moves " +
			"no cursor")
	case d.DB == nil:
		return 0, fmt.Errorf("statelog: a reanchor has no store")
	case d.Domain.ClaimsIdentity() && d.PublishGeneration == nil:
		return 0, fmt.Errorf("statelog: %s claims identity and its reanchor "+
			"publishes no generation record — the record is what a second node "+
			"re-anchoring the same stream meets instead of moving a checkpoint "+
			"of its own", d.Domain.Name())
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
		"domain", d.Domain.Name(), "generation", gen,
		"stream_created_at", in.StreamCreatedAt, "position", in.Position,
		"forced", guard.Force)

	// 4. THE RESET, bounded and resumable.
	if d.ResetVersions != nil {
		if err := d.ResetVersions(ctx, gen); err != nil {
			return 0, fmt.Errorf("statelog: reset %s's versions into generation "+
				"%d: %w", d.Domain.Name(), gen, err)
		}
	}

	// 5. THE RECORD, a claim on the generation's own subject: this node's
	// earlier attempt holds it as this attempt, and anybody else's refuses.
	if d.PublishGeneration != nil {
		if err := d.PublishGeneration(ctx, gen, in); err != nil {
			// ANOTHER REANCHOR HOLDS THIS GENERATION, which is a refusal
			// rather than a failure: nothing here is retried into success,
			// and no checkpoint of this node's has moved.
			var elsewhere *ClaimedElsewhere
			if errors.As(err, &elsewhere) {
				return 0, fmt.Errorf("%w: %s's generation %d is held by another "+
					"reanchor's record: %w", ErrReanchorRefused, d.Domain.Name(),
					gen, err)
			}
			return 0, fmt.Errorf("statelog: publish %s's generation %d: %w",
				d.Domain.Name(), gen, err)
		}
	}

	// 6+7. ONE transaction, the cursor and the audit row.
	//
	// The new cursor is ONE BELOW the live stream's first surviving
	// sequence — zero on a fresh stream — because that is the position
	// from which everything the stream still holds is un-applied.
	cursor := uint64(0)
	if in.FirstSeq > 0 {
		cursor = in.FirstSeq - 1
	}
	if err := d.DB.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		// newTables rather than a literal: a hand-built one leaves
		// whatever field the writer did not think of at its zero value,
		// and every one of them is a key some other side of the
		// framework spells in full.
		t, err := newTables(d.Domain)
		if err != nil {
			return err
		}
		at := Position{Stream: t.stream, Generation: gen, Seq: cursor}
		if err := t.setCursor(ctx, tx, at, in.StreamCreatedAt, now()); err != nil {
			return err
		}
		if d.RecordGeneration == nil {
			return nil
		}
		return d.RecordGeneration(ctx, tx, gen, in)
	}); err != nil {
		return 0, fmt.Errorf("statelog: move %s's cursor into generation %d: %w",
			d.Domain.Name(), gen, err)
	}

	logger.WarnContext(ctx, "statelog_reanchored",
		"domain", d.Domain.Name(), "generation", gen, "cursor", cursor)
	return gen, nil
}
