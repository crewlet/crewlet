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

	// Discard accepts that a RESTORED log's records these rows do not hold —
	// written after the restore, by a node whose rows were the copy's age —
	// are applied nowhere ([ReanchorInputs.Unheld]). Without it such a
	// reanchor refuses, naming the newest of them, because following the log
	// from its end would lose writes somebody was told had landed, with
	// nothing anywhere saying so.
	//
	// ITS OWN FLAG rather than Force: Force answers "I cannot ask the fleet
	// who is most caught up", which says nothing about a log's tail, and an
	// operator forcing past an unreadable register must not discard writes
	// on the same keystroke.
	Discard bool
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
	// checkpoint. Compared with StreamCreatedAt it decides the case: the
	// same instant is the same stream, restored rather than recreated
	// ([ReanchorInputs.Case]). The generation record carries it too, so the
	// audit says which stream the fleet walked away from.
	KeyedTo time.Time

	// FirstSeq is the live stream's first surviving sequence, and LastSeq
	// the last one it wrote — both in the SAME answer as StreamCreatedAt,
	// because together they are what decides the case ([ReanchorCase]) and
	// where its checkpoint goes, and read separately they can straddle a
	// rebuild. A recreated stream's new checkpoint is one below FirstSeq
	// (zero on a fresh stream); a restored one's is LastSeq.
	FirstSeq uint64
	LastSeq  uint64

	// Diverged reports that the log holds, at this node's checkpoint,
	// ANOTHER record than the one the checkpoint names ([ErrLogDiverged]) —
	// read from the checkpoint row and the log's record at its sequence
	// ([CheckpointDiverged]). It is the restored case by another route: the
	// broker came back from an older copy and was written past these rows
	// before this node looked, so the log no longer ENDS below them.
	Diverged bool

	// Unheld is, for the RESTORED case of a domain that claims identity,
	// the newest record on the log that writes rows and that this node's
	// rows do not hold ([UnheldTail]) — nil when there is none. On a
	// restored log such a record was written after the restore, and so was
	// every record above it; following the log from its end applies none of
	// them anywhere, so the transition refuses unless the operator accepts
	// that ([ReanchorGuard.Discard]).
	//
	// NIL FOR EVERY OTHER CASE: a recreated log is followed from its first
	// record and an abandoned one from these rows' own checkpoint, so each
	// applies everything past where it starts. And nil for a domain that
	// claims no identity — the vectors — whose post-restore records are
	// re-embeddings of sources: skipping them keeps these rows' vectors, which
	// describe the sources these rows' own domains keep.
	Unheld *TailRecord

	// Opened is the sequence of THIS node's own record opening the
	// generation this reanchor would open, when an earlier attempt appended
	// it and failed before its checkpoint committed ([OwnGeneration]) — and
	// zero when there is none. The restored case's checkpoint goes one below
	// it, and the walk for Unheld stops there: the record is this node's own
	// transition rather than one written after the restore, and everything
	// beneath it is what the walk exists to find.
	Opened uint64

	// PeersReanchored is how many peers have already re-anchored THIS
	// stream: they stand at a later generation of this domain than this
	// node's checkpoint, and a generation moves only by a reanchor or by
	// adopting the snapshot of a node that ran one.
	//
	// ANY IS A REFUSAL, force or no force, and the reason is not caution:
	// the fleet's history in that generation is the re-anchored peer's
	// rows, and a second reanchor from this node's would open the same
	// generation number over a different prefix of the lost history — the
	// identity claim violated silently and for ever, with no log left to
	// reconcile the two from.
	//
	// A PEER AT THIS NODE'S OWN GENERATION IS NOT ONE. It is on the stream
	// this node's rows came from, or on one it cannot vouch for, and the
	// most-caught-up rule below is what weighs it. It used to be counted
	// here as "hydrated on the live stream" — which every peer still on the
	// lost stream was, so in a fleet of two or more no node could ever
	// re-anchor, and the snapshot the refusal sent the operator to was keyed
	// to the lost stream and adoptable by nobody.
	//
	// NOR IS AN EVICTED PEER. Its rows are not the fleet's history in any
	// generation any more — the eviction says it is not coming back — and
	// counted, a decommissioned node that had re-anchored held every other
	// node's reanchor refused for ever ([ReanchorInputs.Abandoned]).
	PeersReanchored int

	// Position is this node's own committed sequence at the OLD
	// generation, and Highest is the highest any peer published at that
	// same generation ON THE SAME STREAM — the one this node's rows are
	// keyed to, or one a peer's row does not name (a build that did not
	// publish it, weighed conservatively) — among the peers holding history
	// the LOG DOES NOT: whose checkpoint record the log does not hold, past
	// its end or with another record at the sequence, or which name no
	// record ([coord.DomainPosition.CheckpointStoredAt]). A sequence at
	// another generation, or on another stream at this one, is a number in
	// another space and says nothing about who went further along this one;
	// and a peer whose checkpoint record the log holds is on the log, so
	// nothing it applied is lost by a reanchor that follows it. Only the
	// most caught-up node may reanchor, because whatever it did not apply is
	// what the fleet loses — and an EVICTED peer is left out for
	// PeersReanchored's reason: what it applied is on no disk the fleet will
	// read again. HighestPeer names the peer at Highest, for the refusal.
	Position    uint64
	Highest     uint64
	HighestPeer string

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

	// Abandoned is the highest generation of this domain opened by a node
	// the fleet has EVICTED, when that is above Generation, and zero
	// otherwise — read from the evicted node's position row, the trim floor
	// it published and the generation records it left on the log.
	//
	// # What an evicted node's generation is, and why it is neither a guard nor ignored
	//
	// A node that re-anchored a log and was then decommissioned before any
	// peer adopted from it leaves a generation whose history is on no disk
	// the fleet still has: its rows are gone, and what it wrote on the log in
	// that generation was decided from them. Counted as a peer that has
	// already re-anchored — which is what its row says — it refused every
	// remaining node's reanchor for ever, force or no force, while every
	// one of them waited for a donor that did not exist. The operator's
	// eviction of it is the statement that it is not coming back, so its
	// generation stops being the fleet's ([ReanchorInputs.PeersReanchored]
	// and [ReanchorInputs.Highest] leave evicted peers out) — but its NUMBER
	// was used, and its record holds that generation's subject, so the
	// transition opens the generation after it rather than a second history
	// under the same number. And every record written in the generations it
	// skips is VOID where this node follows the log from — decided from rows
	// nobody holds, applied into none ([ReanchorPlan.From]).
	Abandoned uint32

	// ClaimsIdentity is the domain's own [Domain.ClaimsIdentity], which
	// decides whether the two fleet guards apply at all — see
	// [PermitReanchor]. [Reanchor] takes it from the domain rather than
	// from its caller, so the two cannot disagree.
	ClaimsIdentity bool
}

// ReanchorCase is which of the three things that can happen to a log under a
// node's rows a reanchor is answering. They put the new generation's checkpoint
// in different places, and getting that wrong either loses records or applies
// them twice.
//
// # Recreated: the live stream is not the one the rows are keyed to
//
// The stream was deleted and remade, or rebuilt by hand, so it has a new
// creation instant and its sequences started again at 1. The rows hold nothing
// from it, so the checkpoint goes one below its first surviving sequence and
// the domain applies everything it still holds.
//
// # Restored: the live stream IS the one the rows are keyed to, and it is not their history past a point
//
// The broker was brought back from an older copy of its store, which keeps the
// stream's creation instant. Its surviving records are a PREFIX of the history
// the rows were derived from, so the rows already hold every one of them — and
// the checkpoint goes at the log's END. One below the first surviving sequence
// would replay that whole prefix into the new generation, and a record from a
// higher generation outranks every version a row holds: every object would roll
// back to the state it had when the copy was taken, and whatever the rows
// gained since — the tail the copy never had — would be written over.
//
// It is met two ways: the log ENDS below the checkpoint, or it has been
// written past it before this node looked and holds another record at the
// checkpoint's sequence ([ReanchorInputs.Diverged]). The checkpoint goes at
// the end either way, because either way the rows are the history the fleet
// keeps and the log is followed from where it now stands. What that cannot do
// is apply what a node whose rows were the copy's age wrote to the restored log
// before anybody re-anchored it: those records sit below the end, so a
// reanchor over any that write rows these do not hold runs only on the
// operator's word ([ReanchorInputs.Unheld], [ReanchorGuard.Discard]).
//
// # Abandoned: the live stream is the rows' own, and it continues in a generation only an evicted node held
//
// A node re-anchored the log and was decommissioned — and evicted — before any
// peer adopted from it. The log holds every record the rows are missing, so
// neither case above applies, and yet nothing on it can bring the rows level:
// it continues in a generation whose history is on no disk the fleet still
// has. The checkpoint STAYS where the rows are, in the generation after the
// evicted node's, and the domain applies everything the log holds past it —
// except the records written in the generations it skips, which are void
// ([ReanchorPlan.From]): they were decided from the evicted node's rows, and
// applied into these they would mix two histories nothing could separate.
// Everything else past the checkpoint — records the fleet's other nodes wrote
// in this node's own generation before they learned of the move — is the
// history these rows continue, and is kept.
//
// A same-instant stream that ends AT OR PAST the checkpoint, holds there the
// record the checkpoint names, and continues in no abandoned generation is none
// of the three: it holds every record the rows are missing, so there is nothing
// to re-anchor ([ReanchorInputs.Case] refuses it).
//
// A named string for the reason every enum here is one: it travels — in the
// log lines, the API's answer and the CLI's text — and an unknown value off the
// wire is a value rather than a panic.
type ReanchorCase string

const (
	// ReanchorRecreated is a stream with another creation instant than the
	// one the rows are keyed to — or rows keyed to none — so the rows hold
	// none of its records.
	ReanchorRecreated ReanchorCase = "recreated"

	// ReanchorRestored is the stream the rows are keyed to, ending below
	// their checkpoint: a broker brought back from an older copy.
	ReanchorRestored ReanchorCase = "restored"

	// ReanchorAbandoned is the stream the rows are keyed to, continuing in a
	// generation only an evicted node held.
	ReanchorAbandoned ReanchorCase = "abandoned"
)

// Valid reports whether c is one of the three cases.
func (c ReanchorCase) Valid() bool {
	return c == ReanchorRecreated || c == ReanchorRestored || c == ReanchorAbandoned
}

// ReanchorPlan is what a permitted reanchor does: the generation it moves the
// domain to, which case it is answering, and the sequence the new checkpoint
// sits at.
type ReanchorPlan struct {
	Generation uint32
	Case       ReanchorCase
	Cursor     uint64

	// From is the generation the rows stood at before the transition. A
	// record written in a generation strictly between From and Generation
	// was written in a generation this transition ABANDONED — one only an
	// evicted node held — and is void wherever this checkpoint is followed
	// from: consumed, its anchor advanced, and applied into no row. Empty
	// unless the evicted node's generation made the transition skip one
	// ([ReanchorInputs.Abandoned]).
	From uint32

	// Discarded is the newest record the operator accepted discarding — a
	// restored log's record these rows did not hold ([ReanchorGuard.Discard]),
	// applied on no node from here — and nil when the transition discards
	// nothing.
	Discarded *TailRecord
}

// Case is which case these facts describe and where the new checkpoint goes,
// or a refusal when they describe none.
//
// A PURE FUNCTION OF ONE READING, so the status an operator reads before
// confirming and the transition that runs after name the same case from the
// same rule.
//
// The live instant is compared with the rows' own through [IdentityOf], at the
// resolution a checkpoint keeps. Rows keyed to NO instant — a domain with no
// checkpoint — are the recreated case: they hold none of this stream's
// records, whatever it is.
func (in ReanchorInputs) Case() (ReanchorCase, uint64, error) {
	if IdentityOf(in.KeyedTo, in.StreamCreatedAt, !in.KeyedTo.IsZero()) != StreamSame {
		// ONE BELOW THE LIVE STREAM'S FIRST SURVIVING SEQUENCE — zero on a
		// fresh stream — because that is the position from which everything
		// the stream still holds is un-applied here.
		if in.FirstSeq == 0 {
			return ReanchorRecreated, 0, nil
		}
		return ReanchorRecreated, in.FirstSeq - 1, nil
	}
	if pastEnd(in.Position, in.LastSeq) || in.Diverged {
		// AT THE LOG'S END, because the rows already hold every record the
		// restored copy kept — see [ReanchorRestored] for what replaying
		// them into a new generation does. One below this node's own
		// generation record where an earlier attempt already appended it
		// ([ReanchorInputs.Opened]): that record opens the generation, and
		// the resumed applier applies it first. The transition places it
		// below the record it appends in the same way ([Reanchor]).
		if in.Opened > 0 {
			return ReanchorRestored, in.Opened - 1, nil
		}
		return ReanchorRestored, in.LastSeq, nil
	}
	if in.Abandoned > in.Generation {
		// AT THE ROWS' OWN CHECKPOINT: the log holds everything past it,
		// and what it holds in the abandoned generations is void.
		return ReanchorAbandoned, in.Position, nil
	}
	return "", 0, fmt.Errorf("%w: %s is the stream this node's rows are keyed "+
		"to (created %s) and it ends at %d, at or past this node's checkpoint "+
		"at %d, on the record the checkpoint names — it still holds every record "+
		"the rows are missing, so there is "+
		"nothing to re-anchor: the applier follows it, a node below its first "+
		"surviving sequence adopts a peer's snapshot instead, and a node a peer "+
		"re-anchored past adopts from that peer — or, if the peer is gone for "+
		"good, re-anchors once it is evicted",
		ErrReanchorRefused, in.Stream, ConfirmationOf(in.StreamCreatedAt),
		in.LastSeq, in.Position)
}

// Discarding is the refusal a reanchor of these facts makes unless the operator
// accepts discarding what it names ([ReanchorGuard.Discard]), and nil when it
// discards nothing: the restored case over a log holding records these rows do
// not ([ReanchorInputs.Unheld]).
//
// A METHOD OF THE FACTS, beside [ReanchorInputs.Case], so the status an
// operator reads before confirming and the transition that runs after say the
// same thing from the same reading.
func (in ReanchorInputs) Discarding() error {
	// NOTHING TO RE-ANCHOR, OR ANOTHER CASE, DISCARDS NOTHING: Case's own
	// refusal is that reading's answer, and only the restored case skips.
	if which, _, caseErr := in.Case(); caseErr != nil || which != ReanchorRestored ||
		in.Unheld == nil {
		return nil //nolint:nilerr // see above: Case's refusal is reported by Case
	}
	return fmt.Errorf("%w: %s was restored from an older copy and holds records "+
		"this node's rows do not — the newest that writes rows is %s, stored at "+
		"%s. A restored reanchor follows the log from its end, %d, so they would "+
		"be applied on no node: every other node adopts this node's snapshot, and "+
		"what they wrote is lost to whoever it was acknowledged to. Keep them "+
		"instead by not re-anchoring this node: replace its rows with a peer's "+
		"that followed the log (stop it, move its replicated database aside and "+
		"start it again — it replays the log or adopts a snapshot), which gives "+
		"up what only this node's rows hold. Or discard them: re-run with the "+
		"discard flag. %s", ErrReanchorRefused, in.Stream, *in.Unheld,
		in.Unheld.StoredAt.UTC().Format(time.RFC3339Nano), in.LastSeq,
		unheldCaveat(*in.Unheld))
}

// unheldCaveat is what a restored reanchor's refusal says about how sure its
// "not held" is — see tail.go.
func unheldCaveat(r TailRecord) string {
	if !r.LedgerLostBefore.IsZero() {
		return fmt.Sprintf("(This node's operation ledger may have lost rows from "+
			"before %s — to its %s sweep, or with a snapshot from a peer on an "+
			"older build — and this record's operation was minted before that, so "+
			"its rows may hold the record after all: if they do, the discard flag "+
			"discards nothing.)", r.LedgerLostBefore.UTC().Format(time.RFC3339Nano),
			OpsRetention)
	}
	return "(The ledger names every record these rows applied since it last lost " +
		"any, those held from a peer's snapshot included, so this one is not " +
		"among them — unless a gate dropped it, which writes no row; then the " +
		"discard flag discards nothing.)"
}

// PermitReanchor decides whether the transition may run, to which generation,
// and where it puts the checkpoint.
//
// # The two fleet guards belong to a domain that claims identity
//
// A peer that has already re-anchored the stream refuses, and so does a peer
// further along the stream this node's rows came from, because two nodes that
// reanchor independently keep two different prefixes of a history no log holds
// any more — which violates the claim that two nodes at one checkpoint hold the
// same rows. A domain that makes no such claim has
// nothing for either guard to protect: the vectors' per-node coverage differs
// by construction, what one node never applied is a gap in that node's own
// coverage rather than something the fleet loses, and every node re-anchoring
// its own copy is the recovery rather than the hazard. Refusing there would
// leave every node but the first stranded on a stream it can neither read nor
// write, since the one remedy the refusal names — adopting a peer's snapshot —
// replaces every domain's rows to repair one derived index.
func PermitReanchor(in ReanchorInputs, guard ReanchorGuard) (ReanchorPlan, error) {
	live := ConfirmationOf(in.StreamCreatedAt)
	switch {
	case in.StreamCreatedAt.IsZero():
		// NO INSTANT IS NO CONFIRMATION, whatever was typed: the check
		// below would compare against the year one, and the checkpoint
		// would be keyed to a stream nobody can name.
		return ReanchorPlan{}, fmt.Errorf("%w: %s's creation instant is unknown, so there is "+
			"nothing a confirmation could name — read the stream again",
			ErrReanchorRefused, in.Stream)
	case guard.Confirm == "":
		return ReanchorPlan{}, fmt.Errorf("%w: confirm the live stream's creation instant "+
			"(%s) — this verb declares every position this node holds on %s "+
			"stale and there is no undo", ErrReanchorRefused, live, in.Stream)
	case !confirms(guard.Confirm, in.StreamCreatedAt):
		return ReanchorPlan{}, fmt.Errorf("%w: the confirmation names %q and the "+
			"live stream %s was created at %s — running this on the wrong estate "+
			"cannot be undone", ErrReanchorRefused, guard.Confirm, in.Stream, live)
	}
	// WHICH CASE, BEFORE THE FLEET: a stream that holds every record the rows
	// are missing is one no guard below has anything to weigh on.
	which, cursor, err := in.Case()
	if err != nil {
		return ReanchorPlan{}, err
	}
	// AND WHAT FOLLOWING IT FROM THERE WOULD DISCARD, which only the operator
	// may accept — before the fleet guards, because it is a fact about the
	// log whichever node runs the verb.
	if refusal := in.Discarding(); refusal != nil && !guard.Discard {
		return ReanchorPlan{}, refusal
	}
	if in.ClaimsIdentity && in.PeersReanchored > 0 {
		return ReanchorPlan{}, fmt.Errorf("%w: %d peer(s) have already re-anchored "+
			"%s — they stand at a later generation of it than this node's %d. A "+
			"second reanchor from this node's rows would open that generation "+
			"over a different prefix of the lost history, and the identity claim "+
			"would be violated silently and for ever, with no log left to "+
			"reconcile the two from — adopt a re-anchored peer's snapshot instead, "+
			"and if the peer is gone for good, evict it (crewlet retention evict) "+
			"and re-run: an evicted peer's generation is abandoned, not the fleet's",
			ErrReanchorRefused, in.PeersReanchored, in.Stream, in.Generation)
	}
	if in.ClaimsIdentity && !guard.Force {
		if !in.RegisterReadable {
			return ReanchorPlan{}, fmt.Errorf("%w: the positions register could not be read, "+
				"so whether this is the most caught-up node is unknown — and "+
				"whatever it did not apply is what the fleet loses. Re-run with "+
				"the force flag if that is accepted", ErrReanchorRefused)
		}
		if in.Position < in.Highest {
			who, remedy := "the fleet", "re-anchor on the node that did instead"
			if in.HighestPeer != "" {
				who, remedy = in.HighestPeer, "re-anchor on "+in.HighestPeer+" instead"
			}
			return ReanchorPlan{}, fmt.Errorf("%w: this node is at %d and %s reached %d "+
				"holding history the log does not — only the most caught-up node "+
				"may reanchor, because everything above its own position is what "+
				"the reanchor discards; %s", ErrReanchorRefused, in.Position, who,
				in.Highest, remedy)
		}
	}
	// THE GENERATION AFTER EVERY ONE THIS DOMAIN HAS USED: this node's own,
	// and any an evicted node opened ([ReanchorInputs.Abandoned]). That
	// number was used — its record holds that generation's subject — and a
	// second opening of it is two histories under one number.
	base := max(in.Generation, in.Abandoned)
	if base >= MaxGeneration {
		return ReanchorPlan{}, fmt.Errorf("%w: %s is at generation %d, the last "+
			"the packed position can carry", ErrReanchorRefused, in.Stream, base)
	}
	// THE NEW GENERATION IS DERIVED LOCALLY, from this domain's own
	// checkpoint and what the caller already read: this verb runs when the
	// broker estate has been lost, so a generation that needed a further
	// coordination read could not be computed at the one moment it is
	// needed. An abandoned generation only ever raises it.
	plan := ReanchorPlan{Generation: base + 1, Case: which, Cursor: cursor, From: in.Generation}
	if in.Discarding() != nil {
		plan.Discarded = in.Unheld
	}
	return plan, nil
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
	// Generation is the one the transition moves to, and Case what it is
	// answering — which is what the record's own account of why has to say.
	Generation uint32
	Case       ReanchorCase

	// Discarded is the newest record the operator accepted discarding
	// ([ReanchorPlan.Discarded]), nil when the transition discards none.
	Discarded *TailRecord

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

	// GenerationSubject is the subject generation gen's record is published
	// on, and false for a domain that keeps none — what a reader of the log
	// asks to learn which generations were opened, and by whom
	// ([GenerationOpeners]).
	GenerationSubject(gen uint32) (Subject, bool)
}

// ReanchorStream is the live log as the transition needs it: the append, the
// per-subject probe that tells a lost race from nothing at all, a read of the
// record a lost race lost to, and a LIVE reading of the stream's creation
// instant.
//
// DECLARED HERE because the transition is the caller.
type ReanchorStream interface {
	Appender
	LogReader

	// CreatedAt is the stream's creation instant as the broker reports it
	// NOW — read on every call, never cached.
	CreatedAt(ctx context.Context) (time.Time, error)
}

// LogReader reads one record of a log back by its sequence: its subject, its
// bytes and the broker's own instant for it, and false with a nil error for a
// sequence the log no longer holds.
//
// DECLARED HERE because the framework is the caller, and for the paths that
// address the log rather than follow it — a consumer is how records are
// delivered, and nothing here needs one to answer "what is at this sequence".
type LogReader interface {
	At(ctx context.Context, seq uint64) (subject string, payload []byte,
		storedAt time.Time, ok bool, err error)
}

// ReanchorConsumer is this node's own reader of the targeted log, as the
// transition needs it: moved to the new checkpoint before that checkpoint
// commits.
type ReanchorConsumer interface {
	Reset(ctx context.Context, after uint64) error
}

// ReanchorRunner is the targeted domain's applier, as the transition needs it:
// re-keyed to the stream it adopted, and to the record its new checkpoint
// names, once the checkpoint has committed. [Runner.Reanchored] is the
// implementation.
type ReanchorRunner interface {
	Reanchored(at Position, created, storedAt time.Time) error
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

	// CompletionBudget bounds the steps after the generation record is
	// appended, which run on a context DETACHED from the caller's — see
	// [Reanchor] — and zero is [ReanchorCompletionBudget].
	CompletionBudget time.Duration

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence: see loggerOr for what silence cost.
	Logger *slog.Logger
	Now    func() time.Time
}

// ReanchorCompletionBudget is how long a reanchor's steps after its append get
// when the caller names no budget of its own.
//
// TWO MINUTES: the steps are a stream read, the walk of the log's tail, one
// record read, a local transaction — and the rebuild of this node's consumer,
// which is a delete and a create of a replicated object and dominates the rest.
// That is a bring-up of two creates, and two minutes is the ceiling a whole
// bring-up gets on a broker with no peers; a caller on a clustered broker
// passes the clustered bring-up's own, which is longer because each create is a
// raft round trip against peers ([ReanchorDeps.CompletionBudget]).
const ReanchorCompletionBudget = 2 * time.Minute

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
	if d.CompletionBudget <= 0 {
		d.CompletionBudget = ReanchorCompletionBudget
	}
	return d
}

// Reanchor runs one domain's generation transition, and reports what it did.
//
// # Seven steps, and the order is its crash matrix
//
//  1. Check the operator's confirmation against the LIVE stream's instant, and
//     decide the case ([ReanchorCase]): recreated or restored, or neither — a
//     stream that still holds every record the rows are missing, which is
//     refused because there is nothing to re-anchor.
//  2. Refuse while any peer has already re-anchored it, and — when none has —
//     require this to be the most caught-up node on the stream its rows came
//     from. Refuse on a node evicted from the
//     domain, whose every record every applier drops.
//  3. Derive the new generation LOCALLY, from the domain's own checkpoint.
//  4. Append the domain's generation record on the live stream, at an
//     expectation of zero on its own subject. A crash after the append leaves
//     the record on the stream: the re-run derives the SAME generation —
//     nothing below has moved the checkpoint — races itself, is refused, finds
//     its OWN record there and carries on, first-writer-wins used for the one
//     thing it is perfectly suited to. A record another node wrote there is a
//     refusal, force or no force: that node opened the generation from its own
//     rows ([appendGeneration]). Everything after the append runs on a
//     context detached from the caller's, under its own budget: the
//     generation is open once the record is on the log, and a caller giving
//     up must not leave it open with nothing committed. Then read the
//     stream's instant AGAIN: the same instant before and after the append is
//     the same stream throughout, because an instant never comes back. For a
//     restored log, walk its tail again up to one below the record — what
//     landed between the reading and the append is below it too — and refuse,
//     unless the operator accepted discarding, over a record these rows do not
//     hold ([ReanchorInputs.Unheld]).
//  5. Move this node's consumer to the case's checkpoint: one below the
//     stream's first surviving sequence for a recreated one, one below the
//     generation record for a restored one. BEFORE the checkpoint, so a consumer
//     that cannot be moved leaves nothing committed: the broker will not move
//     a consumer's start on its own, and one left at the old checkpoint on a
//     rebuilt stream starts past every record the applier then waits for.
//  6. ONE transaction: the domain's checkpoint, at the new generation and that
//     sequence, keyed to the live instant. A crash before it leaves the domain
//     as it was.
//  7. Re-key the domain's applier to that checkpoint and that instant, which
//     clears the verdict that the stream was recreated or that the checkpoint
//     was past its end — so the engine starts the loop again and the domain
//     resumes without a restart.
//
// Every checkpoint sits below the generation record the transition appended —
// the first sequence was read before the append, and the restored case's is
// placed one below the record itself — so the resumed applier applies that
// record first, whichever case it was.
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
func Reanchor(ctx context.Context, d ReanchorDeps, in ReanchorInputs,
	guard ReanchorGuard) (ReanchorPlan, error) {

	switch {
	case d.Domain == nil:
		return ReanchorPlan{}, errors.New("statelog: a reanchor names no domain, " +
			"and a reanchor moves exactly one — the one whose stream the operator " +
			"confirmed")
	case d.DB == nil:
		return ReanchorPlan{}, errors.New("statelog: a reanchor has no store")
	case d.Stream == nil || d.Record == nil || d.Consumer == nil || d.Runner == nil:
		return ReanchorPlan{}, fmt.Errorf("statelog: a reanchor of %s is missing "+
			"its log, its generation record, its consumer or its applier, and a "+
			"partial one leaves the domain unable to follow the stream it "+
			"adopted", d.Domain.Name())
	case d.NodeID == "":
		return ReanchorPlan{}, fmt.Errorf("statelog: a reanchor of %s has no node "+
			"id — the generation record is stamped with it for the eviction gate",
			d.Domain.Name())
	}
	spec := d.Domain.Stream()
	if in.Stream != spec.Name {
		return ReanchorPlan{}, fmt.Errorf("statelog: a reanchor of %s was handed "+
			"facts about %q, and every one of them has to be about %s's own "+
			"stream (%s)", d.Domain.Name(), in.Stream, d.Domain.Name(), spec.Name)
	}
	t, err := newTables(d.Domain)
	if err != nil {
		return ReanchorPlan{}, err
	}
	d = d.resolved()

	// THE DOMAIN SAYS WHETHER IT CLAIMS IDENTITY, never the caller: it is
	// what decides whether the fleet guards apply, and a caller that
	// filled it wrongly would waive them for the tracker.
	in.ClaimsIdentity = d.Domain.ClaimsIdentity()
	plan, err := PermitReanchor(in, guard)
	if err != nil {
		return ReanchorPlan{}, err
	}
	gen := plan.Generation
	if d.Evicted != nil {
		// THE SAME QUESTION FENCE 0 ASKS BEFORE EVERY APPEND, because the
		// record below is an append: an evicted node's is dropped by every
		// applier, and so is everything it writes after — the reanchor
		// would complete and leave a node whose every write applies
		// nowhere. The remedy is the readmission, first.
		evicted, readErr := d.Evicted(ctx)
		if refusal := evictionRefusal(ctx, evicted, readErr); refusal != nil {
			return ReanchorPlan{}, fmt.Errorf("%w: %s: %w", ErrReanchorRefused,
				d.Domain.Name(), refusal)
		}
	}
	at := Position{Stream: spec.Name, Generation: gen, Seq: plan.Cursor}
	if err = at.Valid(); err != nil {
		return ReanchorPlan{}, err
	}
	d.Logger.WarnContext(ctx, "statelog_reanchor_started",
		"domain", d.Domain.Name(), "generation", gen, "case", string(plan.Case),
		"stream", in.Stream, "stream_created_at", in.StreamCreatedAt,
		"keyed_to", in.KeyedTo, "position", in.Position, "last_seq", in.LastSeq,
		"cursor", plan.Cursor, "from_generation", plan.From, "forced", guard.Force,
		"discarding", discardedSeq(plan))

	// 4. THE RECORD, first-writer-wins, and the instant read again.
	record, keeps, err := d.Record.GenerationRecord(GenerationFacts{
		Generation: gen, Case: plan.Case, Discarded: plan.Discarded, Inputs: in,
		By: d.By, Writer: d.NodeID,
		At: d.Now().UTC(),
	})
	if err != nil {
		return ReanchorPlan{}, fmt.Errorf("statelog: encode %s's generation %d "+
			"record: %w", d.Domain.Name(), gen, err)
	}
	var opened uint64
	if keeps {
		if opened, err = appendGeneration(ctx, d, spec, gen, record); err != nil {
			return ReanchorPlan{}, fmt.Errorf("statelog: publish %s's generation "+
				"%d: %w", d.Domain.Name(), gen, err)
		}
	}

	// FROM HERE ON, NOT THE CALLER'S CONTEXT. The record is on the log, so
	// the generation is open whatever happens next, and every step after it
	// exists to make this node the one the fleet continues from. On the
	// caller's context a client that gave up — the operator's CLI waits a
	// bounded time, and the API runs this on the request's own context —
	// cancelled the consumer's rebuild halfway, leaving the generation open
	// on the log with nothing committed; and a re-run then met its own
	// record. Detached, bounded by its own budget
	// ([ReanchorDeps.CompletionBudget]), the transition finishes or fails on
	// its own account.
	post, cancelPost := context.WithTimeout(context.WithoutCancel(ctx), d.CompletionBudget)
	defer cancelPost()
	if err := stillConfirmed(post, d.Stream, in); err != nil {
		return ReanchorPlan{}, err
	}

	// THE RESTORED CASE FOLLOWS THE LOG FROM ONE BELOW ITS OWN RECORD, not
	// from the end it read: whatever landed between that reading and the
	// append sits below the record, and one earlier attempt's record may be
	// above that reading's end or below it. Placed at the end as read, a
	// record a copy-age node wrote in between was applied at the new
	// generation on top of these rows, and a re-run's checkpoint went above
	// its own record, which was then applied nowhere. And the walk for what
	// that discards is taken again up to there, because the records that
	// landed in between are ones the first walk never saw
	// ([ReanchorInputs.Unheld]).
	if keeps && plan.Case == ReanchorRestored {
		plan.Cursor = opened - 1
		at.Seq = plan.Cursor
		if in.ClaimsIdentity {
			unheld, walkErr := UnheldTail(post, d.Domain, d.DB, d.Stream, in.Generation,
				in.FirstSeq, plan.Cursor)
			if walkErr != nil {
				return ReanchorPlan{}, fmt.Errorf("statelog: %s's generation %d is "+
					"open at sequence %d, and whether the records below it hold "+
					"writes this node's rows do not could not be read — nothing "+
					"else is committed; re-run the reanchor: %w", d.Domain.Name(),
					gen, opened, walkErr)
			}
			if unheld != nil && !guard.Discard {
				return ReanchorPlan{}, fmt.Errorf("%w: %s's generation %d is open at "+
					"sequence %d, and records this node's rows do not hold landed "+
					"below it after the log was read — the newest is %s. Nothing "+
					"else is committed. Re-run with the discard flag to finish "+
					"re-anchoring and apply them on no node; or keep them by "+
					"replacing this node's rows with a peer's, and evict this node "+
					"(crewlet retention evict), which abandons the generation it "+
					"opened", ErrReanchorRefused, d.Domain.Name(), gen, opened, *unheld)
			}
			plan.Discarded = unheld
		}
	}

	// 5. THIS NODE'S CONSUMER, before the checkpoint it resumes from.
	if err := d.Consumer.Reset(post, plan.Cursor); err != nil {
		return ReanchorPlan{}, fmt.Errorf("statelog: move this node's %s consumer "+
			"to sequence %d of the adopted stream — nothing is committed, so "+
			"re-running the reanchor repeats it: %w", d.Domain.Name(),
			plan.Cursor, err)
	}

	// THE RECORD THE NEW CHECKPOINT NAMES — the log's own at its sequence,
	// which is what the applier verifies the log against from here on
	// ([Runner.VerifyCheckpoint]). None where the log holds none there: one
	// below a rebuilt stream's first record, or sequence 0. Read before the
	// transaction because a broker read inside it would hold the writer for
	// a round trip; the sequence is below the generation record, so nothing
	// the log does meanwhile moves it.
	var names time.Time
	if plan.Cursor > 0 {
		_, _, storedAt, ok, readErr := d.Stream.At(post, plan.Cursor)
		if readErr != nil {
			return ReanchorPlan{}, fmt.Errorf("statelog: read %s's record at sequence "+
				"%d, which the new checkpoint names — nothing is committed, so "+
				"re-running the reanchor repeats it: %w", d.Domain.Name(),
				plan.Cursor, readErr)
		}
		if ok {
			names = storedAt
		}
	}

	// 6. THE ONE CHECKPOINT, alone in its transaction.
	if err := d.DB.Tx(post, func(tx *sql.Tx) error {
		return t.reanchorCursor(post, tx, at, in.StreamCreatedAt, names, plan.From, d.Now())
	}); err != nil {
		return ReanchorPlan{}, fmt.Errorf("statelog: move %s's checkpoint into "+
			"generation %d: %w", d.Domain.Name(), gen, err)
	}

	// 7. THE APPLIER, re-keyed to what just committed.
	if err := d.Runner.Reanchored(at, in.StreamCreatedAt, names); err != nil {
		return ReanchorPlan{}, fmt.Errorf("statelog: re-key %s's applier to %s: %w",
			d.Domain.Name(), at, err)
	}

	// THE ONE COMPLETION LINE. Who asked is the API's line (`reanchored`,
	// carrying the operator) and the generation record's, and neither is a
	// fact this function has.
	d.Logger.WarnContext(ctx, "statelog_reanchored",
		"domain", d.Domain.Name(), "generation", gen, "case", string(plan.Case),
		"stream", in.Stream, "stream_created_at", in.StreamCreatedAt,
		"cursor", plan.Cursor, "prev_last_seq_seen", in.Highest,
		"discarded", discardedSeq(plan),
		"detail", reanchoredDetail(plan.Case))
	return plan, nil
}

// discardedSeq is the sequence of the newest record a plan discards, and 0 when
// it discards none — what the two log lines carry, since a line cannot hold the
// record whole.
func discardedSeq(plan ReanchorPlan) uint64 {
	if plan.Discarded == nil {
		return 0
	}
	return plan.Discarded.Seq
}

// reanchoredDetail is the completion line's account of what the transition
// kept and what it could not, which differs by case.
func reanchoredDetail(c ReanchorCase) string {
	const common = "; its applier resumes without a restart, every position " +
		"below this generation is comparable and safely stale, and no other " +
		"domain's checkpoint moved"
	if c == ReanchorRestored {
		return "the log was restored from an older copy: the rows already hold " +
			"every record it kept, so this domain follows it from its end and " +
			"replays none of them" + common + "; what the rows hold past the " +
			"copy is on no log, so every other node adopts this node's snapshot " +
			"rather than replaying"
	}
	if c == ReanchorAbandoned {
		return "the log continued in a generation only an evicted node held: " +
			"this domain follows it from its own checkpoint in the generation " +
			"after that one, and the records written in the generations it " +
			"skipped are void here" + common + "; every other node adopts this " +
			"node's snapshot"
	}
	return "the log was recreated: this domain follows it from the first " +
		"record it holds" + common + "; records that were on the old stream " +
		"and were never applied here are not recovered"
}

// appendGeneration appends a domain's generation record at an expectation of
// zero on its own subject, and establishes that the record the subject then
// holds is THIS node's.
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
//     written by nothing but a reanchor to that generation, and the one record
//     on it is what the arbitration below decides.
//   - THE RESOLUTION is not waited for. The applier that would resolve it is
//     the one this transition is about to point at the stream, and it applies
//     the record the moment it resumes, from the case's checkpoint — which the
//     record is above, because the first sequence and the end that checkpoint
//     was taken from were both read before the append.
//   - THE GATE RESERVE ([GateReserve]) is not asked either, so the record may
//     land in it. It is a gate record in the reserve's sense — it installs the
//     generation every later record is placed in — and the stream it lands on
//     is one every node's fence refuses ordinary writes to until it has, so
//     nothing ordinary can have filled it since, and it lands at most once per
//     generation.
//
// # A record already there is carried on from only if it is this node's own
//
// The subject holds at most one record, so an append that did not land — the
// broker refused the expectation, or its answer never arrived and the probe
// found the subject taken — or that the broker acknowledged as a DUPLICATE of
// a record already in its window has lost to whatever the subject holds. That
// is this node's own earlier attempt when a re-run races itself, and the
// transition carries on: first-writer-wins, used for the one thing it is
// perfectly suited to.
//
// It is ANOTHER node's record when two nodes opened the generation
// independently — the register could not be read and both forced, or each
// read it before the other's row said anything — and carrying on from it was
// the defect. Both committed a checkpoint in the generation, each over its own
// rows: two histories under one number, which the identity claim forbids and
// nothing on the log could ever reconcile, since every node applies the same
// records into whichever rows it holds. So the record is READ BACK and its
// writer compared with this node's, whatever the guard allowed: no force and
// no unreadable register reaches past it, because nothing the operator could
// know makes two openings of one generation safe. The duplicate case needs the
// read too — the broker checks its window before the expectation on a
// clustered stream, so a message id that collided with a peer's would be
// acknowledged as though it had landed; the domains' message ids name their
// writer, and this does not rely on it.
//
// # It answers where the record is
//
// The sequence the record stands at — the one it landed at, or the one the
// subject held it at when this was a re-run finding its own — because that is
// where the restored case's checkpoint goes: one below it.
func appendGeneration(ctx context.Context, d ReanchorDeps, spec StreamSpec, gen uint32,
	record GenerationRecord) (uint64, error) {

	subject := spec.SubjectPrefix + "." + record.Subject.String()
	zero := uint64(0)
	seq, duplicate, err := d.Stream.Append(ctx, subject, record.OpID, &zero, record.Payload)
	switch f, detail := classify(err); f {
	case faultNone:
		if !duplicate {
			// LANDED AT ZERO: the subject held nothing, so nobody's
			// record is there but this one.
			return seq, nil
		}
		return seq, generationIsOurs(ctx, d, subject, seq, gen)
	case faultFull, faultTooLarge, faultRefused:
		return 0, fmt.Errorf("the broker refused to store the record: %s", detail)
	case faultRejected, faultUnknown:
		held, found, probe := d.Stream.LastSeq(ctx, subject)
		switch {
		case probe != nil:
			return 0, fmt.Errorf("the append was not acknowledged (%v) and whether "+
				"%s holds a record could not be read: %w", err, subject, probe)
		case !found:
			return 0, fmt.Errorf("the append was not acknowledged and %s holds "+
				"nothing, so nothing landed — re-run the reanchor: %w", subject, err)
		}
		return held, generationIsOurs(ctx, d, subject, held, gen)
	}
	return 0, err
}

// generationIsOurs reads the record a generation subject holds at seq and
// refuses unless this node wrote it — see [appendGeneration].
//
// EVERY ANSWER BUT "OURS" STOPS THE TRANSITION BEFORE ANYTHING COMMITS: a
// record that cannot be read is an error to re-run, and one that cannot say who
// wrote it is refused, because the one thing this read exists to rule out is a
// record this node cannot vouch for.
func generationIsOurs(ctx context.Context, d ReanchorDeps, subject string, seq uint64,
	gen uint32) error {

	got, payload, _, ok, err := d.Stream.At(ctx, seq)
	switch {
	case err != nil:
		return fmt.Errorf("%s already holds a record, at sequence %d, and it could "+
			"not be read to learn whose it is — nothing was committed; re-run "+
			"the reanchor: %w", subject, seq, err)
	case !ok:
		return fmt.Errorf("%s held a record at sequence %d and the log no longer "+
			"has it, so whose it was is unknown — nothing was committed; re-run "+
			"the reanchor", subject, seq)
	case got != subject:
		return fmt.Errorf("the broker named sequence %d as %s's and holds a record "+
			"of %s there — nothing was committed; re-run the reanchor", seq,
			subject, got)
	}
	env, err := d.Domain.Envelope(payload)
	if err != nil {
		return fmt.Errorf("%w: %s's generation %d is already opened by a record at "+
			"sequence %d of %s whose envelope does not decode (%v), so whether "+
			"this node wrote it cannot be told — and a second opening of one "+
			"generation is the one thing this must not do", ErrReanchorRefused,
			d.Domain.Name(), gen, seq, d.Domain.Stream().Name, err)
	}
	if env.Writer == d.NodeID {
		return nil
	}
	writer := env.Writer
	if writer == "" {
		writer = "a writer that did not say who it was"
	}
	return fmt.Errorf("%w: %s's generation %d was already opened by %s — its "+
		"record is at sequence %d of %s. It opened it from its own rows, and a "+
		"second opening from this node's would put two histories under one "+
		"generation number, which nothing on the log could ever reconcile; "+
		"this node adopts a snapshot from a node in that generation instead",
		ErrReanchorRefused, d.Domain.Name(), gen, writer, seq,
		d.Domain.Stream().Name)
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
