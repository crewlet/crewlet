package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrReanchorRefused reports a generation transition that was not permitted.
var ErrReanchorRefused = errors.New("statelog: reanchor refused")

// A REANCHOR MOVES ONE DOMAIN, and the generation it moves is that domain's.
//
// # Why one, when the verb used to move every cursor
//
// Every fact a reanchor is decided from is a fact about ONE stream — its
// creation instant, its first surviving sequence, this node's position on it
// and the fleet's high-water mark there — and each domain has its own stream,
// its own sequence space and its own generation ([Position] carries the stream
// beside the generation for exactly that reason). The transition used to write
// every registered domain's checkpoint from the one stream the operator named:
// a reanchor of the tracker's log keyed the pages and vector checkpoints to the
// tracker's creation instant and its first sequence, so the next boot found
// both of THOSE streams "recreated", stopped their appliers and refused their
// writes — and reanchoring either of them then did the same to the tracker. An
// operator could never converge.
//
// So the generation is PER DOMAIN, and it always was everywhere else: the
// positions register reports one per domain, the trim publishes one floor per
// domain at that domain's generation, and a join asks for an artefact at each
// domain's own. A reanchor of one stream advances that domain's generation and
// leaves every other domain's checkpoint — its generation, its sequence and the
// instant it is keyed to — exactly where it was.

// ReanchorGuard is what an operator must satisfy before a domain's generation
// moves.
type ReanchorGuard struct {
	// Confirm is what the operator typed, which must name the live stream's
	// own creation instant (see [ConfirmationOf]). It is a guard against
	// running the verb on the wrong estate — the one mistake that cannot be
	// undone.
	Confirm string

	// Force overrides the "only the most caught-up node may reanchor"
	// rule, and its message must name exactly what may be lost.
	Force bool
}

// ConfirmationOf renders a stream's creation instant the way an operator is
// asked to echo it.
//
// ONE SPELLING, and it is the one every surface already prints: the status
// read answers the instant as a JSON time, which is RFC 3339 with nanoseconds;
// the CLI prints that and tells the operator to paste it back; and a
// `wrong_stream` refusal names the live instant at the same precision. The
// check used to compare against whole seconds instead, so the command the CLI
// printed was refused whenever the broker's instant had a fraction — which a
// real one always does.
func ConfirmationOf(created time.Time) string {
	return created.UTC().Format(time.RFC3339Nano)
}

// confirms reports whether what the operator typed names instant, at the
// precision a stream's identity is compared at ([identityResolution]).
//
// PARSED, never compared as text, so a confirmation that names the same
// instant in another zone or with trailing zeros is the same confirmation —
// and at the identity's own resolution, because a checkpoint row keeps
// microseconds while the broker reports nanoseconds, and an instant read back
// from either is still the same stream.
func confirms(typed string, instant time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, typed)
	if err != nil {
		return false
	}
	return at.Truncate(identityResolution).Equal(instant.Truncate(identityResolution))
}

// ReanchorInputs is everything the guards read, and the one stream they were
// read from.
type ReanchorInputs struct {
	// Stream is the log the operator named — the one these facts were
	// read from. No guard reads it; the lines do, because every other
	// field here is a fact about ONE stream, and a line carrying the
	// high-water mark without the stream it belongs to leaves the number
	// with nothing to be read against.
	Stream string

	// StreamCreatedAt is the LIVE stream's creation instant, read in the
	// same answer as FirstSeq. It is what the operator's confirmation is
	// checked against, what a `wrong_stream` refusal names as the live
	// instant, and what the new checkpoint is keyed to — ONE value for all
	// three. An instant sampled when the node booted is none of them once
	// the stream has been rebuilt under a running node: confirming it keys
	// the checkpoint to a stream that no longer exists, and the next boot
	// finds the domain recreated again.
	StreamCreatedAt time.Time

	// KeyedTo is the instant this domain's rows were keyed to before the
	// transition — its checkpoint row's own — and zero where there was no
	// checkpoint. Provenance only: the generation record carries it so the
	// audit says which stream the fleet walked away from.
	KeyedTo time.Time

	// FirstSeq is the live stream's first surviving sequence. The new
	// cursor is one below it, which is zero on a fresh stream.
	FirstSeq uint64

	// PeersHydrated is how many peers are caught up on the LIVE stream —
	// at this domain's generation or a later one, since a peer that has
	// already re-anchored this stream is exactly a peer caught up on it.
	//
	// ANY IS A REFUSAL, and the reason is not caution: two nodes that
	// reanchor independently each keep whatever they applied off the old
	// stream before it vanished, and if those prefixes differ the identity
	// claim is silently violated for ever — with no log left to reconcile
	// them from.
	PeersHydrated int

	// Position is this node's own committed sequence at the OLD
	// generation, and Highest is the highest any node published at that
	// same generation — a sequence at another generation is a number in
	// another space and says nothing about this one. Only the most
	// caught-up node may reanchor when nobody is hydrated, because
	// whatever it did not apply is what the fleet loses.
	Position uint64
	Highest  uint64

	// RegisterReadable reports whether the register could be listed at
	// all. When it could not, the comparison above cannot be made and only
	// an explicit force proceeds.
	RegisterReadable bool

	// Generation is THIS DOMAIN's generation, read LOCALLY from its own
	// checkpoint — no coordination read, because the verb runs when
	// coordination may be exactly what was lost, and the checkpoint is the
	// one value every other reader of the generation takes (the publisher,
	// the applier, the trim and the positions heartbeat).
	Generation uint32

	// ClaimsIdentity is the domain's own [Domain.ClaimsIdentity], which
	// decides whether the two fleet guards apply at all — see
	// [PermitReanchor]. [Reanchor] takes it from the domain rather than
	// from its caller, so the two cannot disagree.
	ClaimsIdentity bool
}

// PermitReanchor decides whether the transition may run, and to which
// generation.
//
// # The two fleet guards belong to a domain that claims identity
//
// A hydrated peer refuses, and so does a peer further along the old stream,
// because two nodes that reanchor independently keep two different prefixes of
// a history no log holds any more — which violates the claim that two nodes at
// one checkpoint hold the same rows. A domain that makes no such claim has
// nothing for either guard to protect: the vectors' per-node coverage differs
// by construction, what one node never applied is a gap in that node's own
// coverage rather than something the fleet loses, and every node re-anchoring
// its own copy is the recovery rather than the hazard. Refusing there would
// leave every node but the first stranded on a stream it can neither read nor
// write, since the one remedy the refusal names — adopting a peer's snapshot —
// replaces every domain's rows to repair one derived index.
func PermitReanchor(in ReanchorInputs, guard ReanchorGuard) (uint32, error) {
	live := ConfirmationOf(in.StreamCreatedAt)
	switch {
	case in.StreamCreatedAt.IsZero():
		// NO INSTANT IS NO CONFIRMATION, whatever was typed: the check
		// below would compare against the year one, and the checkpoint
		// would be keyed to a stream nobody can name.
		return 0, fmt.Errorf("%w: %s's creation instant is unknown, so there is "+
			"nothing a confirmation could name — read the stream again",
			ErrReanchorRefused, in.Stream)
	case guard.Confirm == "":
		return 0, fmt.Errorf("%w: confirm the live stream's creation instant "+
			"(%s) — this verb declares every position this node holds on %s "+
			"stale and there is no undo", ErrReanchorRefused, live, in.Stream)
	case !confirms(guard.Confirm, in.StreamCreatedAt):
		return 0, fmt.Errorf("%w: the confirmation names %q and the live stream "+
			"%s was created at %s — running this on the wrong estate cannot be "+
			"undone", ErrReanchorRefused, guard.Confirm, in.Stream, live)
	}
	if in.ClaimsIdentity && in.PeersHydrated > 0 {
		return 0, fmt.Errorf("%w: %d peer(s) are hydrated on the live stream. "+
			"Two nodes that reanchor independently each keep whatever they "+
			"applied off the old stream before it vanished, and if those "+
			"prefixes differ the identity claim is violated silently and for "+
			"ever, with no log left to reconcile them from — adopt a hydrated "+
			"peer's snapshot instead", ErrReanchorRefused, in.PeersHydrated)
	}
	if in.ClaimsIdentity && !guard.Force {
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
	if in.Generation >= MaxGeneration {
		return 0, fmt.Errorf("%w: %s is at generation %d, the last the packed "+
			"position can carry", ErrReanchorRefused, in.Stream, in.Generation)
	}
	// THE NEW GENERATION IS DERIVED LOCALLY, from this domain's own
	// checkpoint: this verb runs when the broker estate has been lost, so a
	// generation that needed a coordination read could not be computed at
	// the one moment it is needed.
	return in.Generation + 1, nil
}

// GenerationRecord is the one record a reanchor appends: the domain's own
// statement of the transition, on the stream it adopts.
type GenerationRecord struct {
	// Subject is the domain's generation subject for the new generation —
	// the arbitration unit, so two operators deriving the same number
	// contend on it and exactly one record lands.
	Subject Subject

	// OpID is the record's operation id, and its message id.
	OpID string

	// Payload is the record as the domain encodes it.
	Payload []byte
}

// GenerationFacts is what a domain's generation record is written from.
type GenerationFacts struct {
	// Generation is the one the transition moves to.
	Generation uint32

	// Inputs are the facts the transition was decided from.
	Inputs ReanchorInputs

	// By names the operator, and Writer this node.
	By, Writer string

	// At is the authored instant.
	At time.Time
}

// GenerationEncoder is the one step of a reanchor only the targeted domain can
// take: saying, in its own record format, that the transition happened.
//
// DECLARED HERE because the transition is the caller. And REQUIRED of every
// domain rather than optional, because "this domain keeps no record of it" is a
// claim the domain has to make about itself — the vectors make it, and say
// why — rather than something a nil could leave unsaid.
type GenerationEncoder interface {
	// GenerationRecord encodes the record, and reports false for a domain
	// that keeps none.
	GenerationRecord(f GenerationFacts) (GenerationRecord, bool, error)
}

// ReanchorStream is the live log as the transition needs it: the append, the
// per-subject probe that tells a lost race from nothing at all, and a LIVE
// reading of the stream's creation instant.
//
// DECLARED HERE because the transition is the caller.
type ReanchorStream interface {
	Appender

	// CreatedAt is the stream's creation instant as the broker reports it
	// NOW — read on every call, never cached.
	CreatedAt(ctx context.Context) (time.Time, error)
}

// ReanchorConsumer is this node's own reader of the targeted log, as the
// transition needs it: moved to the new checkpoint before that checkpoint
// commits.
type ReanchorConsumer interface {
	Reset(ctx context.Context, after uint64) error
}

// ReanchorRunner is the targeted domain's applier, as the transition needs it:
// re-keyed to the stream it adopted once the checkpoint has committed.
// [Runner.Reanchored] is the implementation.
type ReanchorRunner interface {
	Reanchored(at Position, created time.Time) error
}

// ReanchorDeps is everything the transition needs that it does not own.
type ReanchorDeps struct {
	// Domain is the ONE domain being re-anchored: the one whose stream the
	// operator confirmed. Its checkpoint is the only one this writes.
	Domain Domain

	// Stream is that domain's live log, Record its own generation record,
	// Consumer this node's reader of the log, and Runner its applier.
	Stream   ReanchorStream
	Record   GenerationEncoder
	Consumer ReanchorConsumer
	Runner   ReanchorRunner

	// Evicted reports this node's own eviction from the domain, nil for a
	// domain with no eviction gate — the one the write path's fence 0
	// reads, so the reanchor refuses on exactly what a write would.
	Evicted func(ctx context.Context) (bool, error)

	// DB is the replicated estate, which holds the checkpoint.
	DB Estate

	// By names the operator for the record, and NodeID this node — the
	// writer the eviction gate compares against.
	By     string
	NodeID string

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence: see loggerOr for what silence cost.
	Logger *slog.Logger
	Now    func() time.Time
}

// resolved is these deps with every optional one defaulted, and the only
// value [Reanchor] reads them from.
//
// A METHOD rather than two lines at the top of Reanchor, because Reanchor is
// a function and keeps nothing a test could inspect afterwards: the default
// logger is asserted through this, the way every constructor's is asserted
// through the field it stores.
func (d ReanchorDeps) resolved() ReanchorDeps {
	d.Logger = loggerOr(d.Logger)
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}

// Reanchor runs one domain's generation transition.
//
// # Seven steps, and the order is its crash matrix
//
//  1. Check the operator's confirmation against the LIVE stream's instant.
//  2. Refuse while any peer is hydrated on it, and — when none is — require
//     this to be the most caught-up node. Refuse on a node evicted from the
//     domain, whose every record every applier drops.
//  3. Derive the new generation LOCALLY, from the domain's own checkpoint.
//  4. Append the domain's generation record on the live stream, at an
//     expectation of zero on its own subject. A crash after the append leaves
//     the record on the stream: the re-run derives the SAME generation —
//     nothing below has moved the checkpoint — races itself, is refused, finds
//     the record there and carries on, first-writer-wins used for the one
//     thing it is perfectly suited to. Then read the stream's instant AGAIN:
//     the same instant before and after the append is the same stream
//     throughout, because an instant never comes back.
//  5. Move this node's consumer to one below the stream's first surviving
//     sequence. BEFORE the checkpoint, so a consumer that cannot be moved
//     leaves nothing committed: the broker will not move a consumer's start
//     on its own, and one left at the old checkpoint on a rebuilt stream
//     starts past every record the applier then waits for.
//  6. ONE transaction: the domain's checkpoint, at the new generation and one
//     below the first surviving sequence, keyed to the live instant. A crash
//     before it leaves the domain as it was.
//  7. Re-key the domain's applier to that checkpoint and that instant, which
//     clears the verdict that the stream was recreated — so the engine starts
//     the loop again and the domain resumes without a restart.
//
// # What it no longer does, and why
//
// It rewrote every object's `version` into the new generation and wrote the
// audit row directly, beside the checkpoint. Neither was right. A version is
// not what the broker arbitrates against — the ANCHOR is — and an anchor below
// the current generation is already "no anchor here" to the publisher, which
// asks the broker and publishes at zero under the floor theorem; so a reset
// bought nothing, and it had never once run: its table list named columns the
// schema does not have and a body-revision counter it would have corrupted.
// Run, it would have re-billed every embedding, since a task's vector is
// current while its `source_rev` equals the task's version. And the audit row
// is what the generation record APPLIES as: the relaunched applier reads the
// new stream from its head, the record is on it, and the row is then derived
// like every other — where a row written beside the checkpoint was one no
// replay of the log could reproduce.
func Reanchor(ctx context.Context, d ReanchorDeps, in ReanchorInputs, guard ReanchorGuard) (uint32, error) {
	switch {
	case d.Domain == nil:
		return 0, errors.New("statelog: a reanchor names no domain, and a " +
			"reanchor moves exactly one — the one whose stream the operator " +
			"confirmed")
	case d.DB == nil:
		return 0, errors.New("statelog: a reanchor has no store")
	case d.Stream == nil || d.Record == nil || d.Consumer == nil || d.Runner == nil:
		return 0, fmt.Errorf("statelog: a reanchor of %s is missing its log, its "+
			"generation record, its consumer or its applier, and a partial one "+
			"leaves the domain unable to follow the stream it adopted", d.Domain.Name())
	case d.NodeID == "":
		return 0, fmt.Errorf("statelog: a reanchor of %s has no node id — the "+
			"generation record is stamped with it for the eviction gate",
			d.Domain.Name())
	}
	spec := d.Domain.Stream()
	if in.Stream != spec.Name {
		return 0, fmt.Errorf("statelog: a reanchor of %s was handed facts about %q, "+
			"and every one of them has to be about %s's own stream (%s)",
			d.Domain.Name(), in.Stream, d.Domain.Name(), spec.Name)
	}
	t, err := newTables(d.Domain)
	if err != nil {
		return 0, err
	}
	d = d.resolved()

	// THE DOMAIN SAYS WHETHER IT CLAIMS IDENTITY, never the caller: it is
	// what decides whether the fleet guards apply, and a caller that
	// filled it wrongly would waive them for the tracker.
	in.ClaimsIdentity = d.Domain.ClaimsIdentity()
	gen, err := PermitReanchor(in, guard)
	if err != nil {
		return 0, err
	}
	if d.Evicted != nil {
		// THE SAME QUESTION FENCE 0 ASKS BEFORE EVERY APPEND, because the
		// record below is an append: an evicted node's is dropped by every
		// applier, and so is everything it writes after — the reanchor
		// would complete and leave a node whose every write applies
		// nowhere. The remedy is the readmission, first.
		evicted, readErr := d.Evicted(ctx)
		if refusal := evictionRefusal(ctx, evicted, readErr); refusal != nil {
			return 0, fmt.Errorf("%w: %s: %w", ErrReanchorRefused, d.Domain.Name(), refusal)
		}
	}
	cursor := uint64(0)
	if in.FirstSeq > 0 {
		// ONE BELOW THE LIVE STREAM'S FIRST SURVIVING SEQUENCE — zero on a
		// fresh stream — because that is the position from which
		// everything the stream still holds is un-applied.
		cursor = in.FirstSeq - 1
	}
	at := Position{Stream: spec.Name, Generation: gen, Seq: cursor}
	if err = at.Valid(); err != nil {
		return 0, err
	}
	d.Logger.WarnContext(ctx, "statelog_reanchor_started",
		"domain", d.Domain.Name(), "generation", gen, "stream", in.Stream,
		"stream_created_at", in.StreamCreatedAt, "keyed_to", in.KeyedTo,
		"position", in.Position, "forced", guard.Force)

	// 4. THE RECORD, first-writer-wins, and the instant read again.
	record, keeps, err := d.Record.GenerationRecord(GenerationFacts{
		Generation: gen, Inputs: in, By: d.By, Writer: d.NodeID, At: d.Now().UTC(),
	})
	if err != nil {
		return 0, fmt.Errorf("statelog: encode %s's generation %d record: %w",
			d.Domain.Name(), gen, err)
	}
	if keeps {
		if err := appendGeneration(ctx, d.Stream, spec, record); err != nil {
			return 0, fmt.Errorf("statelog: publish %s's generation %d: %w",
				d.Domain.Name(), gen, err)
		}
	}
	if err := stillConfirmed(ctx, d.Stream, in); err != nil {
		return 0, err
	}

	// 5. THIS NODE'S CONSUMER, before the checkpoint it resumes from.
	if err := d.Consumer.Reset(ctx, cursor); err != nil {
		return 0, fmt.Errorf("statelog: move this node's %s consumer to sequence %d "+
			"of the adopted stream — nothing is committed, so re-running the "+
			"reanchor repeats it: %w", d.Domain.Name(), cursor, err)
	}

	// 6. THE ONE CHECKPOINT, alone in its transaction.
	if err := d.DB.Tx(ctx, func(tx *sql.Tx) error {
		return t.setCursor(ctx, tx, at, in.StreamCreatedAt, d.Now())
	}); err != nil {
		return 0, fmt.Errorf("statelog: move %s's checkpoint into generation %d: %w",
			d.Domain.Name(), gen, err)
	}

	// 7. THE APPLIER, re-keyed to what just committed.
	if err := d.Runner.Reanchored(at, in.StreamCreatedAt); err != nil {
		return 0, fmt.Errorf("statelog: re-key %s's applier to %s: %w",
			d.Domain.Name(), at, err)
	}

	// THE ONE COMPLETION LINE. Who asked is the API's line (`reanchored`,
	// carrying the operator) and the generation record's, and neither is a
	// fact this function has.
	d.Logger.WarnContext(ctx, "statelog_reanchored",
		"domain", d.Domain.Name(), "generation", gen, "stream", in.Stream,
		"stream_created_at", in.StreamCreatedAt, "cursor", cursor,
		"prev_last_seq_seen", in.Highest,
		"detail", "this domain now follows the adopted stream from its head, "+
			"its applier resumes without a restart, and every position below "+
			"this generation is comparable and safely stale; no other domain's "+
			"checkpoint moved, and records that were on the old stream and "+
			"were never applied here are not recovered")
	return gen, nil
}

// appendGeneration appends a domain's generation record at an expectation of
// zero on its own subject.
//
// # Why this is not an ordinary write through the domain's publisher
//
// Because every fence the write authority runs is a statement about the stream
// this node's rows are keyed to, and the one write a reanchor makes is the one
// whose purpose is to leave that stream. Fence 0 refuses while the runner holds
// the recreation verdict — which is the only state a reanchor is ever run in —
// and the zero fence clears an expectation of zero against a checkpoint and a
// floor from the old history, which on a rebuilt log end below the checkpoint
// and refuse. What the publisher's fences would establish, this establishes
// differently, and soundly:
//
//   - THE IDENTITY is the operator's confirmation of the LIVE instant, checked
//     before this runs and read again after it ([stillConfirmed]). An instant
//     is never reissued, so the same instant on both sides of the append is the
//     same stream throughout it.
//   - THE EVICTION is asked by [Reanchor] before this, from the same reader
//     fence 0 asks.
//   - THE FLOOR THEOREM is not in play. It protects an object whose record was
//     trimmed from a writer whose rows are stale; the generation subject is
//     written by nothing but a reanchor to that generation, whose every
//     record says the same thing, so the worst a stale expectation of zero can
//     do here is land a second copy of it — and the audit row it applies as is
//     create-only.
//   - THE RESOLUTION is not waited for. The applier that would resolve it is
//     the one this transition is about to point at the stream, and it applies
//     the record the moment it resumes, from one below the stream's first
//     sequence — which the record is above, because that first sequence was
//     read before the append.
//
// A REFUSAL IS THE SUBJECT ALREADY HOLDING THE RECORD — a re-run racing its own
// earlier append, or the operator who got there first — and the transition
// carries on, which is what first-writer-wins is for. An answer that is no
// answer is resolved by asking the subject, exactly as the write authority
// resolves one.
func appendGeneration(ctx context.Context, stream ReanchorStream, spec StreamSpec,
	record GenerationRecord) error {

	subject := spec.SubjectPrefix + "." + record.Subject.String()
	zero := uint64(0)
	_, _, err := stream.Append(ctx, subject, record.OpID, &zero, record.Payload)
	switch f, detail := classify(err); f {
	case faultNone:
		return nil
	case faultFull:
		return fmt.Errorf("the broker refused to store the record: %s", detail)
	case faultRejected, faultUnknown:
		_, found, probe := stream.LastSeq(ctx, subject)
		switch {
		case probe != nil:
			return fmt.Errorf("the append was not acknowledged (%v) and whether "+
				"%s holds a record could not be read: %w", err, subject, probe)
		case !found:
			return fmt.Errorf("the append was not acknowledged and %s holds "+
				"nothing, so nothing landed — re-run the reanchor: %w", subject, err)
		}
		return nil
	}
	return err
}

// stillConfirmed reads the stream's instant again and refuses unless it is the
// one the operator confirmed.
//
// AFTER THE APPEND, which is the read that makes the append's identity a fact
// rather than a hope: the confirmation was checked against a reading taken
// before it, and a stream rebuilt in between would have taken the record
// under a name the operator never looked at. The checkpoint is keyed to the
// confirmed instant, so it must not commit over a stream that is not it.
func stillConfirmed(ctx context.Context, stream ReanchorStream, in ReanchorInputs) error {
	live, err := stream.CreatedAt(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s's creation instant could not be read again after "+
			"the generation record, so which stream it went to is unknown and "+
			"nothing was committed — re-run the reanchor: %w",
			ErrReanchorRefused, in.Stream, err)
	}
	if IdentityOf(in.StreamCreatedAt, live, true) != StreamSame {
		return fmt.Errorf("%w: %s was created at %s when this reanchor read it and "+
			"at %s now — it was rebuilt again while the reanchor ran, and nothing "+
			"was committed; confirm the new instant and re-run",
			ErrReanchorRefused, in.Stream, ConfirmationOf(in.StreamCreatedAt),
			ConfirmationOf(live))
	}
	return nil
}
