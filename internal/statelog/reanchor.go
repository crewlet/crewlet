package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// ErrReanchorRefused reports a generation transition that was not permitted.
var ErrReanchorRefused = errors.New("statelog: reanchor refused")

// refuseReanchor is a reanchor refused for the reason format says, told to the
// operator in those words ([Telling]) and standing on [ErrReanchorRefused].
func refuseReanchor(format string, args ...any) error {
	return &Telling{Say: fmt.Sprintf(format, args...), Err: ErrReanchorRefused}
}

// stoppedReanchor is a reanchor that stopped on a fault, told to the operator
// as format says (what was committed and what to do) with the fault kept for
// the log.
func stoppedReanchor(fault error, format string, args ...any) error {
	return &Telling{Say: fmt.Sprintf(format, args...), Err: fault}
}

// A REANCHOR MOVES ONE DOMAIN, and the generation it moves is that domain's.
// Every fact it is decided from (the creation instant, the first surviving
// sequence, this node's position, the fleet's high-water mark) is about ONE
// stream, and each domain has its own stream, sequence space and generation
// ([Position] carries the stream beside the generation). Keying every domain's
// checkpoint to the one stream named makes the others' next boot find them
// "recreated", and reanchoring those does the same back: no convergence. So a
// reanchor advances one domain's generation and leaves every other domain's
// checkpoint (generation, sequence, keyed instant) where it was, as the
// positions register, the trim's floors and a join's artefacts already do.

// ReanchorGuard is what an operator must satisfy before a domain's generation
// moves.
type ReanchorGuard struct {
	// Confirm is what the operator typed, which must name the live stream's
	// own creation instant ([ConfirmationOf]): a guard against running the
	// verb on the wrong estate, the one mistake that cannot be undone.
	Confirm string

	// Force overrides the "only the most caught-up node may reanchor"
	// rule, and its message must name exactly what may be lost.
	Force bool

	// Discard accepts that a RESTORED log's records these rows do not hold
	// (written after the restore by a node whose rows were the copy's age)
	// are applied nowhere ([ReanchorInputs.Unheld]). Without it such a
	// reanchor refuses, naming the newest, since following the log from its
	// end silently loses acknowledged writes. Not Force: Force answers "I
	// cannot ask the fleet who is most caught up", which says nothing about
	// a log's tail, so forcing must not also discard.
	Discard bool
}

// ConfirmationOf renders a stream's creation instant the way an operator is
// asked to echo it: RFC 3339 with nanoseconds, the one spelling every surface
// prints (the status read's JSON time, the CLI, a `wrong_stream` refusal).
// Whole seconds would refuse what the CLI printed, since a real broker's
// instant has a fraction.
func ConfirmationOf(created time.Time) string {
	return created.UTC().Format(time.RFC3339Nano)
}

// confirms reports whether what the operator typed names instant, at the
// precision a stream's identity is compared at ([identityResolution]). Parsed,
// not compared as text, so another zone or trailing zeros still confirm; and
// truncated, since a checkpoint row keeps microseconds while the broker
// reports nanoseconds.
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
	// Stream is the log the operator named, the one these facts were read
	// from. No guard reads it; the log lines carry it so a high-water mark
	// has a stream to be read against.
	Stream string

	// StreamCreatedAt is the LIVE stream's creation instant, read in the
	// same answer as FirstSeq: ONE value the confirmation is checked
	// against, a `wrong_stream` refusal names and the new checkpoint is
	// keyed to. An instant sampled at boot is stale once the stream is
	// rebuilt under a running node, and keying to it makes the next boot
	// find the domain recreated again.
	StreamCreatedAt time.Time

	// KeyedTo is the instant this domain's checkpoint row was keyed to
	// before the transition, zero with no checkpoint. Equal to
	// StreamCreatedAt, the case is restored rather than recreated
	// ([ReanchorInputs.Case]). The generation record carries it, so the
	// audit names the stream the fleet walked away from.
	KeyedTo time.Time

	// FirstSeq is the live stream's first surviving sequence and LastSeq
	// the last it wrote, both in the SAME answer as StreamCreatedAt: they
	// decide the case ([ReanchorCase]) and its checkpoint, and read
	// separately they can straddle a rebuild. A recreated stream's
	// checkpoint is one below FirstSeq (zero on a fresh stream); a restored
	// one's is LastSeq.
	FirstSeq uint64
	LastSeq  uint64

	// Diverged reports that the log holds, at this node's checkpoint,
	// another record than the one the checkpoint names ([ErrLogDiverged],
	// [CheckpointDiverged]): the restored case by another route, a broker
	// back from an older copy and written past these rows before this node
	// looked.
	Diverged bool

	// Unheld is, for the RESTORED case of a domain that claims identity,
	// the newest record on the log that writes rows these rows do not hold
	// ([UnheldTail]), nil when none. It and every record above it came
	// after the restore, and following the log from its end applies none of
	// them, so the transition refuses without [ReanchorGuard.Discard].
	//
	// Nil for every other case: a recreated log is followed from its first
	// record and an abandoned one from these rows' checkpoint, so each
	// applies everything past its start. Nil for a domain claiming no
	// identity (the vectors, the usage days): each node re-anchors its own
	// copy, and the vectors' skipped re-embeddings describe sources these
	// rows already keep.
	Unheld *TailRecord

	// Opened is the sequence of THIS node's own record opening the
	// generation this reanchor would open, left by an earlier attempt that
	// failed before its checkpoint committed ([OwnGeneration]), zero when
	// none. The restored case's checkpoint goes one below it, and the walk
	// for Unheld stops there: it is this node's transition, not a write
	// made after the restore.
	Opened uint64

	// ReanchoredPeers are the peers, by node id, that already re-anchored
	// THIS stream: they stand at a later generation of this domain than
	// this node's checkpoint. By name, because the refusal sends the
	// operator to one of them (adopt its snapshot, or evict it if gone).
	// ANY refuses, force or no force: a second reanchor would open the same
	// generation over a different prefix of the lost history, violating
	// the identity claim for ever with no log to reconcile from.
	//
	// A peer at this node's own generation is not one: the most-caught-up
	// rule below weighs it, and counted here no fleet of two could ever
	// re-anchor. Nor is an EVICTED peer: counted, a decommissioned one
	// would refuse every reanchor for ever ([ReanchorInputs.Abandoned]).
	ReanchoredPeers []string

	// Position is this node's committed sequence at the OLD generation,
	// and Highest the highest any peer published at that generation ON THE
	// SAME STREAM (the one these rows are keyed to, or any when unkeyed),
	// counting only peers holding history the LOG DOES NOT: whose
	// checkpoint record the log lacks or holds another record for, or which
	// name none ([coord.DomainPosition.CheckpointStoredAt]). Another
	// generation or stream is another number space, and a peer on the log
	// loses nothing. Only the most caught-up node may reanchor, since what
	// it did not apply the fleet loses; evicted peers are left out as in
	// ReanchoredPeers. HighestPeer names the peer at Highest, for the refusal.
	Position    uint64
	Highest     uint64
	HighestPeer string

	// RegisterReadable reports whether the register could be listed at
	// all. When it could not, the comparison above cannot be made and only
	// an explicit force proceeds.
	RegisterReadable bool

	// Generation is THIS DOMAIN's generation, read LOCALLY from its own
	// checkpoint: the verb runs when coordination may be what was lost, and
	// the checkpoint is what every other reader of the generation takes.
	Generation uint32

	// Abandoned is the highest generation of this domain opened by a node
	// the fleet has EVICTED, when above Generation, else zero: read from the
	// evicted node's position row, its trim floor and its generation records
	// on the log. That generation's history is on no disk the fleet has, so
	// it is no guard (ReanchoredPeers and Highest leave evicted peers out,
	// or every node waits for ever on a donor that is gone), yet not
	// ignored: its NUMBER was used, so the transition opens the one after
	// it, and records written in the skipped generations are VOID where
	// this node follows the log from ([ReanchorPlan.From]).
	Abandoned uint32

	// ClaimsIdentity is the domain's own [Domain.ClaimsIdentity], which
	// decides whether the two fleet guards apply ([PermitReanchor]).
	// [Reanchor] takes it from the domain, never its caller.
	ClaimsIdentity bool
}

// ReanchorCase is which of the three things that can happen to a log under a
// node's rows a reanchor is answering. Each puts the new generation's
// checkpoint in a different place, and the wrong place loses records or
// applies them twice. A same-instant stream that ends at or past the
// checkpoint, holds there the record it names, and continues in no abandoned
// generation is none of them: there is nothing to re-anchor
// ([ReanchorInputs.Case] refuses it). A named string, so an unknown value off
// the wire is a value rather than a panic.
type ReanchorCase string

const (
	// ReanchorRecreated is a stream with another creation instant than the
	// one the rows are keyed to, or rows keyed to none: deleted and remade
	// or rebuilt, its sequences restarted. The rows hold none of its
	// records, so the checkpoint goes one below its first surviving sequence.
	ReanchorRecreated ReanchorCase = "recreated"

	// ReanchorRestored is the stream the rows are keyed to, brought back
	// from an older copy: it ends below their checkpoint, or was written
	// past it and holds another record there ([ReanchorInputs.Diverged]).
	// Its records are a PREFIX of the rows' history, so the checkpoint goes
	// at the log's END: replayed into a higher generation they outrank
	// every row's version and roll every object back to the copy. What
	// copy-age nodes wrote to the restored log sits below the end, so a
	// reanchor over any these rows lack runs only on the operator's word
	// ([ReanchorInputs.Unheld], [ReanchorGuard.Discard]).
	ReanchorRestored ReanchorCase = "restored"

	// ReanchorAbandoned is the stream the rows are keyed to, continuing in a
	// generation only an evicted node held (it re-anchored and was evicted
	// before any peer adopted from it). The checkpoint STAYS where the rows
	// are, in the generation after the evicted node's, and the domain
	// applies everything past it except records written in the skipped
	// generations, which are void ([ReanchorPlan.From]): applied here they
	// would mix two histories. Records other nodes wrote in this node's own
	// generation are the history these rows continue, and are kept.
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
	// (one only an evicted node held, [ReanchorInputs.Abandoned]) is void
	// wherever this checkpoint is followed from: consumed, its anchor
	// advanced, applied into no row.
	From uint32

	// Discarded is the newest record the operator accepted discarding (a
	// restored log's record these rows did not hold, [ReanchorGuard.Discard]),
	// applied on no node from here, and nil when nothing is discarded.
	Discarded *TailRecord

	// StaleAfter is, for the RESTORED case, the sequence of the generation
	// record the transition appended, else zero. A record after it written
	// in a generation below Generation is void wherever this checkpoint is
	// followed from ([ReasonOvertaken]): other nodes write in the old
	// generation until they learn of the move, decided from rows the
	// reanchor did not keep. An abandoned reanchor keeps such records, since
	// there the other nodes' rows are the history these continue.
	StaleAfter uint64
}

// Case is which case these facts describe and where the new checkpoint goes,
// or a refusal when they describe none. A pure function of one reading, so
// the status read before confirming and the transition after agree. Rows
// keyed to no instant (no checkpoint) are the recreated case.
func (in ReanchorInputs) Case() (ReanchorCase, uint64, error) {
	if IdentityOf(in.KeyedTo, in.StreamCreatedAt, !in.KeyedTo.IsZero()) != StreamSame {
		// ONE BELOW THE FIRST SURVIVING SEQUENCE (zero on a fresh stream):
		// everything the stream still holds is un-applied here.
		if in.FirstSeq == 0 {
			return ReanchorRecreated, 0, nil
		}
		return ReanchorRecreated, in.FirstSeq - 1, nil
	}
	if pastEnd(in.Position, in.LastSeq) || in.Diverged {
		// AT THE LOG'S END: the rows hold every record the restored copy
		// kept ([ReanchorRestored]). One below this node's own generation
		// record where an earlier attempt appended it ([ReanchorInputs.Opened]),
		// so the resumed applier applies it first, as [Reanchor] places it.
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
	// NAME WHO MOVED IT ON, where somebody did: a node a peer re-anchored
	// past acts on that peer.
	onward := "the applier follows it, a node below its first surviving " +
		"sequence adopts a peer's snapshot instead, and a node a peer " +
		"re-anchored past adopts from that peer (or, if the peer is gone for " +
		"good, re-anchors once it's evicted)"
	if len(in.ReanchoredPeers) > 0 {
		have, snapshot, evict := reanchoredPast(in.ReanchoredPeers)
		onward = fmt.Sprintf("%s re-anchored it past this node. Adopt %s instead, "+
			"or evict a peer that's gone for good (%s) and re-run", have, snapshot, evict)
	}
	return "", 0, refuseReanchor("%s is the stream this node's rows are keyed "+
		"to (created %s) and it ends at %d, at or past this node's checkpoint "+
		"at %d, on the record the checkpoint names. It still holds every record "+
		"the rows are missing, so there's nothing to re-anchor: %s",
		in.Stream, ConfirmationOf(in.StreamCreatedAt), in.LastSeq, in.Position, onward)
}

// reanchoredPast names the peers that re-anchored a log past this node, as the
// subject of "have", with the snapshot this node adopts from them and the
// eviction that releases a peer gone for good, in the number the set has.
func reanchoredPast(peers []string) (have, snapshot, evict string) {
	if len(peers) == 1 {
		return words.And(peers) + " has", "its snapshot",
			fmt.Sprintf("`crewlet retention evict %s -confirm %s`", peers[0], peers[0])
	}
	return words.And(peers) + " have", "a snapshot from one of them",
		"`crewlet retention evict <node> -confirm <node>` for each"
}

// Discarding is the refusal a reanchor of these facts makes unless the operator
// accepts discarding what it names ([ReanchorGuard.Discard]), and nil when it
// discards nothing: the restored case over a log holding records these rows do
// not ([ReanchorInputs.Unheld]). A method of the facts, like
// [ReanchorInputs.Case], so the status and the transition agree.
func (in ReanchorInputs) Discarding() error {
	// ONLY THE RESTORED CASE DISCARDS; Case reports its own refusal.
	if which, _, caseErr := in.Case(); caseErr != nil || which != ReanchorRestored ||
		in.Unheld == nil {
		return nil //nolint:nilerr // see above: Case's refusal is reported by Case
	}
	return refuseReanchor("%s was restored from an older copy and holds records "+
		"this node's rows don't. The newest that writes rows is %s, stored at "+
		"%s. A restored reanchor follows the log from its end, %d, so they would "+
		"be applied on no node: every other node adopts this node's snapshot, and "+
		"what they wrote is lost to whoever it was acknowledged to. Keep them "+
		"instead by not re-anchoring this node: replace its rows with a peer's "+
		"that followed the log (stop it, move its replicated database aside and "+
		"start it again: it replays the log or adopts a snapshot), which gives "+
		"up what only this node's rows hold. Or discard them: re-run with the "+
		"discard flag. %s", in.Stream, *in.Unheld,
		in.Unheld.StoredAt.UTC().Format(time.RFC3339Nano), in.LastSeq,
		unheldCaveat(*in.Unheld))
}

// unheldCaveat is what a restored reanchor's refusal says about how sure its
// "not held" is — see tail.go.
func unheldCaveat(r TailRecord) string {
	if !r.LedgerLostBefore.IsZero() {
		return fmt.Sprintf("(This node's operation ledger may have lost rows from "+
			"before %s to a retention sweep, and this record's operation was minted "+
			"before that, so "+
			"its rows may hold the record after all: if they do, the discard flag "+
			"discards nothing.)", r.LedgerLostBefore.UTC().Format(time.RFC3339Nano))
	}
	return "(The ledger names every record these rows applied since it last lost " +
		"any, those held from a peer's snapshot included, so this one is not " +
		"among them, unless a gate dropped it, which writes no row; then the " +
		"discard flag discards nothing.)"
}

// PermitReanchor decides whether the transition may run, to which generation,
// and where it puts the checkpoint.
//
// The two fleet guards (a peer already re-anchored, a peer further along)
// apply only to a domain that claims identity: two independent reanchors keep
// two prefixes of a history no log holds, so two nodes at one checkpoint
// would hold different rows. Without the claim (the vectors), coverage
// differs per node by construction, each node re-anchoring its own copy is
// the recovery, and the refusal's remedy (adopting a peer's snapshot) would
// replace every domain's rows to repair one derived index.
func PermitReanchor(in ReanchorInputs, guard ReanchorGuard) (ReanchorPlan, error) {
	live := ConfirmationOf(in.StreamCreatedAt)
	switch {
	case in.StreamCreatedAt.IsZero():
		// NO INSTANT IS NO CONFIRMATION: the check below would compare
		// against the year one, keying the checkpoint to an unnamed stream.
		return ReanchorPlan{}, refuseReanchor("%s's creation instant is unknown, so there's "+
			"nothing a confirmation could name. Read the stream again", in.Stream)
	case guard.Confirm == "":
		return ReanchorPlan{}, refuseReanchor("confirm the live stream's creation instant "+
			"(%s). This declares every position this node holds on %s "+
			"stale, and there's no undo", live, in.Stream)
	case !confirms(guard.Confirm, in.StreamCreatedAt):
		return ReanchorPlan{}, refuseReanchor("the confirmation names %q and the "+
			"live stream %s was created at %s. Running this on the wrong estate "+
			"can't be undone", guard.Confirm, in.Stream, live)
	}
	// WHICH CASE, BEFORE THE FLEET: a stream holding every record the rows
	// are missing leaves no guard below anything to weigh.
	which, cursor, err := in.Case()
	if err != nil {
		return ReanchorPlan{}, err
	}
	// THEN WHAT IT WOULD DISCARD, which only the operator may accept: before
	// the fleet guards, as a fact about the log whichever node runs the verb.
	if refusal := in.Discarding(); refusal != nil && !guard.Discard {
		return ReanchorPlan{}, refusal
	}
	if peers := in.ReanchoredPeers; in.ClaimsIdentity && len(peers) > 0 {
		have, snapshot, evict := reanchoredPast(peers)
		they := "it stands"
		if len(peers) > 1 {
			they = "they stand"
		}
		return ReanchorPlan{}, refuseReanchor("%s already re-anchored %s: %s at a "+
			"later generation of it than this node's %d. A second reanchor from "+
			"this node's rows would open that generation over a different prefix "+
			"of the lost history, and the identity claim would be violated "+
			"silently and for ever, with no log left to reconcile the two from. "+
			"Adopt %s instead. A peer that's gone for good is evicted (%s) before "+
			"you re-run: an evicted peer's generation is abandoned, not the "+
			"fleet's", have, in.Stream, they, in.Generation, snapshot, evict)
	}
	if in.ClaimsIdentity && !guard.Force {
		if !in.RegisterReadable {
			return ReanchorPlan{}, refuseReanchor("the positions register couldn't be read, " +
				"so whether this is the most caught-up node is unknown, and " +
				"whatever it didn't apply is what the fleet loses. Re-run with " +
				"the force flag if that's accepted")
		}
		if in.Position < in.Highest {
			who, remedy := "the fleet", "re-anchor on the node that did instead"
			if in.HighestPeer != "" {
				who, remedy = in.HighestPeer, "re-anchor on "+in.HighestPeer+" instead"
			}
			return ReanchorPlan{}, refuseReanchor("this node is at %d and %s reached %d "+
				"holding history the log doesn't. Only the most caught-up node "+
				"may reanchor, because everything above its own position is what "+
				"the reanchor discards; %s", in.Position, who, in.Highest, remedy)
		}
	}
	// THE GENERATION AFTER EVERY ONE THIS DOMAIN HAS USED, an evicted node's
	// included ([ReanchorInputs.Abandoned]): its record holds that
	// generation's subject, and a second opening is two histories under one
	// number.
	base := max(in.Generation, in.Abandoned)
	if base >= MaxGeneration {
		return ReanchorPlan{}, refuseReanchor("%s is at generation %d, the last "+
			"the packed position can carry", in.Stream, base)
	}
	// DERIVED LOCALLY, from this domain's checkpoint and what the caller
	// read: the verb runs when the broker estate is lost, so it cannot need
	// a coordination read. An abandoned generation only raises it.
	plan := ReanchorPlan{Generation: base + 1, Case: which, Cursor: cursor, From: in.Generation}
	if in.Discarding() != nil {
		plan.Discarded = in.Unheld
	}
	return plan, nil
}

// GenerationRecord is the one record a reanchor appends: the domain's own
// statement of the transition, on the stream it adopts.
type GenerationRecord struct {
	// Subject is the domain's generation subject for the new generation:
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
	// answering, which the record's account of why has to say.
	Generation uint32
	Case       ReanchorCase

	// Discarded is the newest record the operator accepted discarding
	// ([ReanchorPlan.Discarded]), nil when the transition discards none.
	Discarded *TailRecord

	// Inputs are the facts the transition was decided from.
	Inputs ReanchorInputs

	// By names the operator as the audit trail records them, ByKind their
	// actor kind (the vocabulary [iam.ActorFor] records under) and
	// OperatorID the credential they acted through: the three columns every
	// write carries, read for every domain by [GenerationFacts.Author].
	// Writer is this node.
	By         string
	ByKind     iam.ActorKind
	OperatorID string
	Writer     string

	// At is the authored instant.
	At time.Time
}

// Author is the party a generation record names as its author, in the actor
// vocabulary ([iam.ActorKind]) every domain's audit columns use, and false
// where the facts name nobody (a blank By): then the record is the node's,
// under [GenerationFacts.Writer], never an empty author column. One mapping for every domain, since one
// reanchor names one party and the audit feed reads the histories side by
// side; a domain only maps the kind into its own columns. A kind the
// vocabulary does not hold (the zero value included) is an OPERATOR, as
// [iam.ActorFor] reads it. An operator with no credential is a credential
// acting under its own login, so its name stands in for the credential there
// only: under another kind it would put a seat's or person's handle there.
func (f GenerationFacts) Author() (iam.Actor, bool) {
	if strings.TrimSpace(f.By) == "" {
		return iam.Actor{}, false
	}
	kind := f.ByKind
	if !kind.Valid() {
		kind = iam.ActorOperator
	}
	operator := f.OperatorID
	if operator == "" && kind == iam.ActorOperator {
		operator = f.By
	}
	return iam.Actor{Name: f.By, Kind: kind, OperatorID: operator}, true
}

// GenerationEncoder is the one step of a reanchor only the targeted domain can
// take: saying, in its own record format, that the transition happened.
// Declared here because the transition is the caller. Required of every
// domain: "keeps no record" is a claim the domain makes (the vectors and the
// usage log do), not something a nil leaves unsaid.
type GenerationEncoder interface {
	// GenerationRecord encodes the record, and reports false for a domain
	// that keeps none.
	GenerationRecord(f GenerationFacts) (GenerationRecord, bool, error)

	// GenerationSubject is the subject generation gen's record is published
	// on, and false for a domain that keeps none: how a reader of the log
	// learns which generations were opened, and by whom ([GenerationOpeners]).
	GenerationSubject(gen uint32) (Subject, bool)
}

// ReanchorStream is the live log as the transition needs it: the append, the
// per-subject probe that tells a lost race from nothing at all, a read of the
// record a lost race lost to, and a LIVE reading of the stream's creation
// instant. Declared here because the transition is the caller.
type ReanchorStream interface {
	Appender
	// VerifiedReader because the transition decodes what it reads back
	// (whose record opened a generation, the tail its rows do not hold) and
	// the log holds signed frames.
	VerifiedReader

	// CreatedAt is the stream's creation instant ([StreamCreatedAt]) as a
	// reading reports it NOW: read on every call, never cached.
	CreatedAt(ctx context.Context) (time.Time, error)
}

// LogReader reads one record of a log back by its sequence: its subject, its
// bytes and the broker's own instant for it, and false with a nil error for a
// sequence the log no longer holds. Declared here because the framework is
// the caller, for the paths that address the log rather than follow it with
// a consumer.
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
	// Domain is the domain being re-anchored, and Spec the ONE LOG of it
	// whose stream the operator confirmed: its checkpoint is the only one
	// this writes, since a generation is a coordinate in one stream's number
	// space and a domain's other logs keep theirs.
	Domain Domain
	Spec   StreamSpec

	// Stream is that domain's live log, Record its own generation record,
	// Consumer this node's reader of the log, and Runner its applier.
	Stream   ReanchorStream
	Record   GenerationEncoder
	Consumer ReanchorConsumer
	Runner   ReanchorRunner

	// Evicted reports this node's own eviction from the domain, nil for a
	// domain with no eviction gate: the reader the write path's fence 0
	// asks, so the reanchor refuses on exactly what a write would.
	Evicted func(ctx context.Context) (bool, error)

	// DB is the replicated estate, which holds the checkpoint.
	DB Estate

	// By, ByKind and OperatorID name the operator for the record
	// ([GenerationFacts.By]); all three travel, or the record misnames the
	// actor's kind and credential. NodeID is this node, the writer the
	// eviction gate compares against.
	By         string
	ByKind     iam.ActorKind
	OperatorID string
	NodeID     string

	// CompletionBudget bounds the steps after the generation record is
	// appended, which run on a context DETACHED from the caller's
	// ([Reanchor]); zero is [ReanchorCompletionBudget].
	CompletionBudget time.Duration

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence (loggerOr).
	Logger *slog.Logger
	Now    func() time.Time
}

// ReanchorCompletionBudget is how long a reanchor's steps after its append get
// when the caller names no budget of its own.
//
// TWO MINUTES: the steps are dominated by rebuilding this node's consumer (a
// delete and a create of a replicated object), and two minutes is a whole
// bring-up's ceiling on a broker with no peers. A clustered caller passes its
// clustered bring-up's longer budget, each create there being a raft round
// trip ([ReanchorDeps.CompletionBudget]).
const ReanchorCompletionBudget = 2 * time.Minute

// resolved is these deps with every optional one defaulted, and the only
// value [Reanchor] reads them from: a method so a test can assert the
// defaults, since Reanchor keeps nothing to inspect.
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
// The step order is its crash matrix: decide ([PermitReanchor], then the
// eviction gate), append the generation record ([appendGeneration]), move
// this node's consumer, commit the checkpoint alone, and re-key the applier,
// which clears the recreated or past-end verdict without a restart. A crash
// before the commit leaves the domain as it was, and a re-run finds its own
// record. Every checkpoint sits below that record, so the resumed applier
// applies it first, and the audit row is derived from it rather than written
// beside the checkpoint, where no replay could reproduce it. Object versions
// are not rewritten: the broker arbitrates on the ANCHOR, and one below the
// current generation is already "no anchor" under the floor theorem.
func Reanchor(ctx context.Context, d ReanchorDeps, in ReanchorInputs,
	guard ReanchorGuard) (ReanchorPlan, error) {

	switch {
	case d.Domain == nil:
		return ReanchorPlan{}, errors.New("statelog: a reanchor names no domain, " +
			"and a reanchor moves exactly one: the one whose stream the operator " +
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
			"id, and the generation record is stamped with it for the eviction gate",
			d.Domain.Name())
	}
	spec := d.Spec
	if err := spec.Instantiates(d.Domain); err != nil {
		return ReanchorPlan{}, err
	}
	if in.Stream != spec.Name {
		return ReanchorPlan{}, fmt.Errorf("statelog: a reanchor of %s was handed "+
			"facts about %q, and every one of them has to be about %s's own "+
			"stream (%s)", d.Domain.Name(), in.Stream, d.Domain.Name(), spec.Name)
	}
	t, err := newTables(d.Domain, spec)
	if err != nil {
		return ReanchorPlan{}, err
	}
	d = d.resolved()

	// THE DOMAIN SAYS WHETHER IT CLAIMS IDENTITY, never the caller: a
	// caller filling it wrongly would waive the fleet guards.
	in.ClaimsIdentity = d.Domain.ClaimsIdentity()
	plan, err := PermitReanchor(in, guard)
	if err != nil {
		return ReanchorPlan{}, err
	}
	gen := plan.Generation
	if d.Evicted != nil {
		// FENCE 0's QUESTION, since the record below is an append: every
		// applier drops an evicted node's records, so the reanchor would
		// leave a node whose every write applies nowhere. Readmit first.
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

	// THE RECORD, first-writer-wins at an expectation of zero on its own
	// subject. A crash after it leaves the record: the re-run derives the
	// same generation and finds its own record ([appendGeneration]).
	record, keeps, err := d.Record.GenerationRecord(GenerationFacts{
		Generation: gen, Case: plan.Case, Discarded: plan.Discarded, Inputs: in,
		By: d.By, ByKind: d.ByKind, OperatorID: d.OperatorID, Writer: d.NodeID,
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

	// FROM HERE ON, NOT THE CALLER'S CONTEXT: the record is on the log, so
	// the generation is open whatever happens, and a caller giving up (the
	// CLI waits a bounded time; the API runs this on the request's context)
	// must not leave it open with nothing committed. Bounded by
	// [ReanchorDeps.CompletionBudget]. Then the instant is read again.
	post, cancelPost := context.WithTimeout(context.WithoutCancel(ctx), d.CompletionBudget)
	defer cancelPost()
	if err := stillConfirmed(post, d.Stream, in); err != nil {
		return ReanchorPlan{}, err
	}

	// THE RESTORED CASE FOLLOWS THE LOG FROM ONE BELOW ITS OWN RECORD, not
	// the end it read: what a copy-age node wrote in between would otherwise
	// apply at the new generation over these rows, and a re-run's checkpoint
	// would sit above its own record, applying it nowhere. The walk for what
	// that discards is taken again up to there, since the first walk never
	// saw those records ([ReanchorInputs.Unheld]).
	if keeps && plan.Case == ReanchorRestored {
		plan.Cursor, plan.StaleAfter = opened-1, opened
		at.Seq = plan.Cursor
		if in.ClaimsIdentity {
			unheld, walkErr := UnheldTail(post, d.Domain, spec, d.DB, d.Stream, in.Generation,
				in.FirstSeq, plan.Cursor)
			if walkErr != nil {
				return ReanchorPlan{}, stoppedReanchor(walkErr, "%s's generation %d is "+
					"open at sequence %d, and this node couldn't read whether the "+
					"records below it hold writes its rows lack. Nothing else is "+
					"committed; re-run the reanchor", d.Domain.Name(),
					gen, opened)
			}
			if unheld != nil && !guard.Discard {
				return ReanchorPlan{}, refuseReanchor("%s's generation %d is open at "+
					"sequence %d, and records this node's rows don't hold landed "+
					"below it after the log was read. The newest is %s. Nothing "+
					"else is committed. Re-run with the discard flag to finish "+
					"re-anchoring and apply them on no node; or keep them by "+
					"replacing this node's rows with a peer's, and evict this node "+
					"(crewlet retention evict), which abandons the generation it "+
					"opened", d.Domain.Name(), gen, opened, *unheld)
			}
			plan.Discarded = unheld
		}
	}

	// THIS NODE'S CONSUMER, BEFORE THE CHECKPOINT, so one that cannot be
	// moved leaves nothing committed: the broker never moves a consumer's
	// start itself, and one left at the old checkpoint on a rebuilt stream
	// starts past every record the applier waits for.
	if err := d.Consumer.Reset(post, plan.Cursor); err != nil {
		return ReanchorPlan{}, stoppedReanchor(err, "this node's %s consumer "+
			"couldn't be moved to sequence %d of the adopted stream. Nothing is "+
			"committed, so re-running the reanchor repeats it", d.Domain.Name(),
			plan.Cursor)
	}

	// THE RECORD THE NEW CHECKPOINT NAMES, which the applier verifies the
	// log against from here on ([Runner.VerifyCheckpoint]); none one below a
	// rebuilt stream's first record or at 0. Read before the transaction so
	// no broker round trip holds the writer; the sequence is below the
	// generation record, so nothing moves it meanwhile.
	var names time.Time
	if plan.Cursor > 0 {
		_, _, storedAt, ok, readErr := d.Stream.At(post, plan.Cursor)
		if readErr != nil {
			return ReanchorPlan{}, stoppedReanchor(readErr, "%s's record at sequence "+
				"%d, which the new checkpoint names, couldn't be read. Nothing is "+
				"committed, so re-running the reanchor repeats it", d.Domain.Name(),
				plan.Cursor)
		}
		if ok {
			// IN THE STORE'S ENCODING ([AppliedInstant]), like every path
			// setting the checkpoint's instant: the runner keeps it until
			// its next commit, and a stance or `retention status` read must
			// not see the broker's nanoseconds in the node's zone.
			names = AppliedInstant(storedAt)
		}
	}

	// THE ONE CHECKPOINT, alone in its transaction.
	if err := d.DB.Tx(post, func(tx *sql.Tx) error {
		return t.reanchorCursor(post, tx, at, in.StreamCreatedAt, names, plan.From,
			plan.StaleAfter, d.Now())
	}); err != nil {
		return ReanchorPlan{}, fmt.Errorf("statelog: move %s's checkpoint into "+
			"generation %d: %w", d.Domain.Name(), gen, err)
	}

	// THE APPLIER, re-keyed to what just committed.
	if err := d.Runner.Reanchored(at, in.StreamCreatedAt, names); err != nil {
		return ReanchorPlan{}, fmt.Errorf("statelog: re-key %s's applier to %s: %w",
			d.Domain.Name(), at, err)
	}

	// THE ONE COMPLETION LINE. Who asked is in the API's `reanchored` line
	// and the generation record, not a fact this function has.
	d.Logger.WarnContext(ctx, "statelog_reanchored",
		"domain", d.Domain.Name(), "generation", gen, "case", string(plan.Case),
		"stream", in.Stream, "stream_created_at", in.StreamCreatedAt,
		"cursor", plan.Cursor, "prev_last_seq_seen", in.Highest,
		"discarded", discardedSeq(plan),
		"detail", reanchoredDetail(plan.Case))
	return plan, nil
}

// discardedSeq is the sequence of the newest record a plan discards, and 0 when
// it discards none: what the two log lines carry of it.
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
// zero on its own subject, establishes that the record the subject then holds
// is THIS node's ([generationIsOurs]), and answers its sequence (where it
// landed, or where a re-run found its own): the restored case's checkpoint
// goes one below it.
//
// Not a write through the publisher: its fences are about the stream these
// rows are keyed to, and this write exists to leave it (fence 0 refuses under
// the recreation verdict, the only state a reanchor runs in). Identity,
// eviction, the floor theorem, resolution and the gate reserve are each
// settled otherwise (package doc, "A reanchor's generation record skips the
// publisher").
func appendGeneration(ctx context.Context, d ReanchorDeps, spec StreamSpec, gen uint32,
	record GenerationRecord) (uint64, error) {

	subject := wireSubject(spec.SubjectPrefix, record.Subject)
	zero := uint64(0)
	seq, duplicate, err := d.Stream.Append(ctx, subject, record.OpID, &zero, record.Payload)
	switch f, detail := classify(err); f {
	case faultNone:
		if !duplicate {
			// LANDED AT ZERO: the subject held nothing but this record.
			return seq, nil
		}
		return seq, generationIsOurs(ctx, d, subject, seq, gen)
	// EACH REFUSAL WITH ITS OWN REMEDY: a full log is cleared by raising its
	// ceiling, the other two by nothing a re-run changes. What the broker
	// said goes to this node's log lines.
	case faultFull:
		return 0, stoppedReanchor(errors.New(detail), "%s is at its ceiling, so the "+
			"broker refused the generation record and nothing landed. Raise the "+
			"ceiling with `crewlet retention set-capacity %s <bytes> -confirm <bytes>`, "+
			"then re-run the reanchor", spec.Name, spec.Name)
	case faultTooLarge:
		return 0, stoppedReanchor(errors.New(detail), "the broker refused the "+
			"generation record as larger than %s takes, so nothing landed, and "+
			"re-running the reanchor won't change that. The lines this node "+
			"logged name the limit", spec.Name)
	case faultRefused:
		return 0, stoppedReanchor(errors.New(detail), "the broker refused to store "+
			"the generation record on %s, so nothing landed, and re-running the "+
			"reanchor won't change that. The lines this node logged say what the "+
			"broker answered", spec.Name)
	case faultRejected, faultUnknown:
		held, found, probe := d.Stream.LastSeq(ctx, subject)
		switch {
		case probe != nil:
			return 0, stoppedReanchor(fmt.Errorf("the append was not acknowledged "+
				"(%w) and whether %s holds a record could not be read: %w", err,
				subject, probe), "the generation record wasn't acknowledged, and "+
				"whether it landed couldn't be read. Re-run the reanchor")
		case !found:
			return 0, stoppedReanchor(err, "the generation record wasn't "+
				"acknowledged and nothing landed. Re-run the reanchor")
		}
		return held, generationIsOurs(ctx, d, subject, held, gen)
	}
	return 0, err
}

// generationIsOurs reads the record a generation subject holds at seq and
// refuses unless this node wrote it. The subject holds one record, so an
// append refused, unanswered or acknowledged as a DUPLICATE lost to it: a
// re-run's own record is carried on from, but another node's means two nodes
// opened the generation from their own rows, two histories under one number
// that no log reconciles, so no force or unreadable register reaches past
// this. Duplicates need the read too: a clustered broker checks its dedupe
// window before the expectation, so a colliding message id is acked as landed.
// Every answer but "ours" stops before anything commits: an unreadable record
// is an error to re-run, and one that cannot say who wrote it is refused.
func generationIsOurs(ctx context.Context, d ReanchorDeps, subject string, seq uint64,
	gen uint32) error {

	got, payload, _, ok, err := d.Stream.At(ctx, seq)
	switch {
	case err != nil:
		return stoppedReanchor(err, "the generation already holds a record, at "+
			"sequence %d, and it couldn't be read to learn whose it is. Nothing "+
			"was committed; re-run the reanchor", seq)
	case !ok:
		return stoppedReanchor(fmt.Errorf("%s held a record at sequence %d and "+
			"the log no longer has it", subject, seq), "the generation held a "+
			"record at sequence %d and the log no longer has it, so whose it was "+
			"is unknown. Nothing was committed; re-run the reanchor", seq)
	case got != subject:
		return stoppedReanchor(fmt.Errorf("the broker named sequence %d as %s's "+
			"and holds a record of %s there", seq, subject, got), "the broker "+
			"named sequence %d as the generation's and holds another record "+
			"there. Nothing was committed; re-run the reanchor", seq)
	}
	env, err := d.Domain.Envelope(payload)
	if err != nil {
		return &Telling{Say: fmt.Sprintf("%s's generation %d is already opened by "+
			"a record at sequence %d of %s that this build can't read, so whether "+
			"this node wrote it can't be told, and a second opening of one "+
			"generation is the one thing this must not do", d.Domain.Name(), gen,
			seq, d.Spec.Name), Err: fmt.Errorf("%w: %w", ErrReanchorRefused, err)}
	}
	if env.Writer == d.NodeID {
		return nil
	}
	writer := env.Writer
	if writer == "" {
		writer = "a writer that did not say who it was"
	}
	return refuseReanchor("%s's generation %d was already opened by %s: its "+
		"record is at sequence %d of %s. It opened it from its own rows, and a "+
		"second opening from this node's would put two histories under one "+
		"generation number, which nothing on the log could ever reconcile; "+
		"this node adopts a snapshot from a node in that generation instead",
		d.Domain.Name(), gen, writer, seq, d.Spec.Name)
}

// stillConfirmed reads the stream's instant again and refuses unless it is the
// one the operator confirmed. Called after the append: the confirmation was
// checked against an earlier reading, and a stream rebuilt in between took the
// record under a name the operator never saw, so the checkpoint keyed to the
// confirmed instant must not commit. An instant is never reissued, so the same
// one on both sides is the same stream throughout.
func stillConfirmed(ctx context.Context, stream ReanchorStream, in ReanchorInputs) error {
	live, err := stream.CreatedAt(ctx)
	if err != nil {
		return &Telling{Say: fmt.Sprintf("%s's creation instant couldn't be read "+
			"again after the generation record, so which stream it went to is "+
			"unknown and nothing was committed. Re-run the reanchor", in.Stream),
			Err: fmt.Errorf("%w: %w", ErrReanchorRefused, err)}
	}
	if IdentityOf(in.StreamCreatedAt, live, true) != StreamSame {
		return refuseReanchor("%s was created at %s when this reanchor read it and "+
			"at %s now. It was rebuilt again while the reanchor ran, and nothing "+
			"was committed; confirm the new instant and re-run",
			in.Stream, ConfirmationOf(in.StreamCreatedAt), ConfirmationOf(live))
	}
	return nil
}
