package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// CONTRACT 1: A STREAM'S IDENTITY, and what a consumer must do when it moves.
//
// # The failure this exists for
//
// A stream that is deleted and remade restarts its sequences at 1. Every
// position a node holds then names a number in a space that no longer exists —
// and the new stream is EMPTY, so a consumer that reads it sees nothing and
// reports nothing. That is the shape of the failure: not an error, not a
// refusal, but a successful read of a stream that has forgotten everything.
//
// The broker's own creation instant is what detects it, and it is the only
// thing that can: a sequence cannot, because a recreated stream's sequences
// are perfectly plausible; a message count cannot, because an empty stream and
// an emptied one are the same count.
//
// # Why it is a named contract rather than a check each consumer writes
//
// Every consumer of a durable stream inherits this hazard, whether or not it
// is a state-log domain. [Runner] carries the instant beside its checkpoint
// and answers with a generation; a COMPACTED changelog has no checkpoint and
// no generation to answer with, so its only honest answer is to refuse the
// work that depended on the stream. Both need the same comparison, and a
// comparison written twice is one that is right in one place.

// StreamState is what a consumer has learned about a stream's identity.
type StreamState int

const (
	// StreamFirstSight is a stream this node has no record of. It is NOT a
	// recreation: a fresh node, a fresh company and a newly added consumer
	// all reach it, and refusing there would make a first boot an outage.
	StreamFirstSight StreamState = iota

	// StreamSame is the stream this node last saw.
	StreamSame

	// StreamRecreated is a DIFFERENT stream wearing the same name. Every
	// position below is meaningless and everything the old one held is
	// gone.
	StreamRecreated

	// StreamUnknown is a creation instant that could not be established —
	// the broker did not answer, or answered a zero instant.
	//
	// ITS OWN VALUE, because it is not "same". A consumer that read it as
	// same would resume against a stream it cannot identify, which is the
	// whole failure this contract is about; one that read it as recreated
	// would tear down a healthy node over a broker blip.
	StreamUnknown
)

// String renders the state for a log line.
func (s StreamState) String() string {
	switch s {
	case StreamFirstSight:
		return "first_sight"
	case StreamSame:
		return "same"
	case StreamRecreated:
		return "recreated"
	}
	return "unknown"
}

// identityResolution is the precision at which two observations of one instant
// are the same observation.
//
// A MICROSECOND, because that is what [store.EncodeTime] keeps: an instant
// written and read back is truncated there, while the broker reports its
// stream's creation with nanosecond precision. Comparing the two exactly makes
// EVERY boot after the first report a recreation — a node that refuses to
// hydrate any seat, for ever, over a difference nothing can see.
const identityResolution = time.Microsecond

// IdentityOf compares a stream's current creation instant against the one this
// node recorded.
//
// A PURE FUNCTION OVER TWO INSTANTS, so every consumer reaches the same verdict
// and the verdict is testable without a broker.
func IdentityOf(recorded, current time.Time, known bool) StreamState {
	if current.IsZero() {
		return StreamUnknown
	}
	if !known || recorded.IsZero() {
		return StreamFirstSight
	}
	if current.Truncate(identityResolution).Equal(recorded.Truncate(identityResolution)) {
		return StreamSame
	}
	return StreamRecreated
}

// ErrStreamRecreated reports a stream that is not the one this node last saw.
//
// A SENTINEL, because the caller's response is specific and is never a retry:
// what it holds derived from that stream is stale in a way no re-read repairs,
// and the honest move is to refuse the work rather than to serve an empty
// answer.
var ErrStreamRecreated = errors.New("statelog: the stream was recreated")

// foreignStream is a stream an applier's positions do not belong to: the
// instant of the one its rows were derived from, and the instant of the one the
// broker now serves under the same name.
type foreignStream struct {
	keyed, live time.Time
}

// err is the one sentence every refusal over a foreign stream carries — the
// applier's stop, a read's `wrong_stream` and a write's — so an operator reads
// the same two instants and the same remedy wherever they meet it.
//
// THE REMEDY IS THE OPERATOR'S, and never a retry: what this node holds is
// derived from a history the live log no longer carries, so a re-read reads the
// same mismatch, and the decision to follow the rebuilt stream from its head —
// declaring whatever the old one held and this node never applied lost — is
// not one a node may take on its own.
func (f foreignStream) err(domain, stream string) error {
	return fmt.Errorf("%w: %s's rows are keyed to the stream created at %s and "+
		"the broker's %s was created at %s, so every position this node holds — "+
		"its checkpoint, every arbitration anchor, every version — names a "+
		"sequence that stream counts differently, and neither a read nor a write "+
		"can be answered from them; an operator re-anchors it with `crewlet "+
		"retention reanchor -stream %s`",
		ErrStreamRecreated, domain, f.keyed.UTC().Format(time.RFC3339Nano),
		stream, f.live.UTC().Format(time.RFC3339Nano), stream)
}

// ErrAheadOfLog reports a checkpoint past the log's own last sequence: a
// position the log has never reached, and therefore a position in a history
// the log does not hold.
//
// ITS OWN SENTINEL BESIDE [ErrStreamRecreated], because the creation instant
// cannot show it. A broker restored from a copy older than this node's rows —
// a disk snapshot of the store directory, an embedded broker brought back from
// yesterday's volume — keeps the stream it had, instant and all, so every
// identity check passes, and the one thing that differs is where the log ENDS.
// Both refuse with the one word `wrong_stream` and take the one remedy; the
// sentinel is what lets a caller tell which finding it was without parsing
// prose.
var ErrAheadOfLog = errors.New("statelog: this node's checkpoint is past the log's end")

// ErrGenerationPassed reports a domain a peer has re-anchored while this node's
// rows stayed in the generation before: the fleet's history now continues from
// the re-anchored peer's rows, which this node's are not.
//
// ITS OWN SENTINEL BESIDE [ErrStreamRecreated] and [ErrAheadOfLog], because
// neither of them can see it and its remedy is not theirs. On a recreated
// stream this node's own reading of the instant finds the rebuild, and on a
// restored broker its checkpoint is past the log's end — but only for as long
// as nothing has written past it, and a node whose checkpoint was below the
// restored end never sees either: its log looks like its own history, its
// applier runs on into the new generation's records, and its reads and writes
// are served from rows missing everything the re-anchoring node held past the
// copy. The remedy is not an operator's verb either, because the history this
// node needs exists — on the peer that re-anchored — so the node ADOPTS that
// peer's snapshot, on its own, through the ordinary join.
var ErrGenerationPassed = errors.New("statelog: a peer re-anchored this log past this node's generation")

// ErrLogDiverged reports a log whose record at this node's checkpoint is not the
// record this node consumed there: the log continues a history these rows are
// not.
//
// ITS OWN SENTINEL BESIDE [ErrAheadOfLog], because it is the finding that one
// could not make. A broker restored from an older copy is seen from a node
// whose rows are newer only while its log ENDS below their checkpoint; once
// anything writes it past that — a node whose own rows were not ahead of the
// copy — the end is an ordinary end again, and a node booting then never saw
// it below at all. What still separates the two histories is the record at
// the checkpoint's sequence, which on one stream never changes: the checkpoint
// names it ([Runner.VerifyCheckpoint]), and a different record there means
// everything past it was decided from rows these are not. Unlike a reading of
// the end it cannot come from a member that has not caught up — a member that
// lacks a record answers that it has none, never with another one — so it is
// STICKY: nothing on the log can bring these rows level, and applying the log
// past the checkpoint would put a second history on top of them.
var ErrLogDiverged = errors.New("statelog: the log's record at this node's checkpoint is not the one it consumed")

// divergedLog is what established that a log diverged from an applier's rows:
// the checkpoint, the broker instant of the record the checkpoint names, and
// the instant of the record the log holds at the same sequence.
type divergedLog struct {
	at             Position
	consumed, held time.Time
}

// err is the one sentence every refusal over a diverged log carries — the
// applier's stop, a read's `wrong_stream` and a write's.
func (d divergedLog) err(stream string) error {
	return fmt.Errorf("%w: this node's checkpoint on %s is at sequence %d, on the "+
		"record the broker stored at %s, and the log's record at %d was stored at "+
		"%s — the broker was restored from a copy older than these rows and has "+
		"since been written past them, so the log continues a history they are "+
		"not; nothing past the checkpoint is applied, neither a read nor a write "+
		"can be answered from these rows, and an operator re-anchors them with "+
		"`crewlet retention reanchor -stream %s` (the restored case) or replaces "+
		"them with a peer's",
		ErrLogDiverged, stream, d.at.Seq, d.consumed.UTC().Format(time.RFC3339Nano),
		d.at.Seq, d.held.UTC().Format(time.RFC3339Nano), stream)
}

// ErrLogTruncated reports a log that has lost records a PEER's rows hold: a
// node on the same stream, in the same generation and not evicted, stands past
// the log's end, or says the log diverged from its rows.
//
// ITS OWN SENTINEL, and a finding about the FLEET rather than about this node:
// this node's rows are the log's own history, so its reads are served — but a
// broker restored from an older copy is the one state in which a write from
// here makes things worse whichever history the operator keeps. Kept, the
// peer's rows (the restored reanchor) apply this node's write nowhere, and the
// caller it was acknowledged to has lost it; kept the other way, the peer's rows
// are replaced and nothing needed refusing. So the write waits for the
// operator's choice instead of spending it. An eviction or a readmission is the
// exception ([Request.NodeGate]): evicting the peer is one of the choices.
var ErrLogTruncated = errors.New("statelog: the log lost records a peer's rows hold")

// Truncation is what established that the log lost records a peer's rows hold:
// the peer, its checkpoint, where the log ends, and whether the peer reported
// the log diverged from its rows rather than standing past its end.
type Truncation struct {
	Peer     string
	Seq      uint64
	Last     uint64
	Diverged bool
}

// err is the one sentence every refusal over a truncated log carries.
func (t Truncation) err(stream string) error {
	what := fmt.Sprintf("stands at sequence %d of %s, which ends at %d", t.Seq, stream, t.Last)
	if t.Diverged {
		what = fmt.Sprintf("reports that %s holds, at its checkpoint %d, another "+
			"record than the one it consumed there", stream, t.Seq)
	}
	return fmt.Errorf("%w: peer %s %s — the broker was restored from a copy older "+
		"than that node's rows, which hold records the log lost; a write from here "+
		"would be applied nowhere if the operator keeps those rows (a restored "+
		"reanchor of that node), so this node refuses its writes of the log until "+
		"the operator decides — `crewlet retention reanchor -stream %s` on that "+
		"node, its rows replaced with a peer's, or its eviction (`crewlet "+
		"retention evict`) — and serves its reads, which are the log's own history",
		ErrLogTruncated, t.Peer, what, stream)
}

// sameRecord reports whether two broker instants name one record at one
// sequence — at the resolution a checkpoint row keeps, for the reason
// [identityResolution] gives.
func sameRecord(a, b time.Time) bool {
	return a.Truncate(identityResolution).Equal(b.Truncate(identityResolution))
}

// passedGeneration is what established that a peer re-anchored a domain past
// this applier's rows: the checkpoint the rows stand at, and the generation the
// fleet is on.
type passedGeneration struct {
	at    Position
	fleet uint32
}

// err is the one sentence every refusal over a passed generation carries — the
// applier's stop, a read's `wrong_stream` and a write's.
func (p passedGeneration) err(domain, stream string) error {
	return fmt.Errorf("%w: %s's rows are at generation %d of %s and a peer has "+
		"re-anchored it to generation %d, so the history the log now continues "+
		"is that peer's rows rather than these — neither a read nor a write can be "+
		"answered from them, and nothing on the log can bring them level; this "+
		"node adopts a snapshot from a peer at generation %d, which it asks the "+
		"fleet for on its own",
		ErrGenerationPassed, domain, p.at.Generation, stream, p.fleet, p.fleet)
}

// pastEnd reports a checkpoint past a log's last sequence — [Health.AheadOfLog],
// the zero fence's own reading and [Runner.ObserveEnd] ask it in one spelling.
//
// ONE PREDICATE for the reason [Replayable] is one: the question is asked on
// the read path, on the write path and by the verdict both consult, and three
// spellings of one inequality are three places for its boundary to drift. A
// checkpoint EQUAL to the end is not past it — it is a node that has applied
// the log's last record, which is every caught-up node there is.
func pastEnd(checkpoint, last uint64) bool { return checkpoint > last }

// aheadOfLog is one reading that found an applier's checkpoint past its log's
// end: the checkpoint the reading was paired with, and the end it reported.
type aheadOfLog struct {
	at   Position
	last uint64
}

// err is the one sentence every refusal over a checkpoint past the log's end
// carries — a write's at fence 0 and at the zero fence alike — so an operator
// reads the same two numbers and the same remedy wherever they meet it.
//
// IT NAMES WHY NO WRITE CAN BE ANSWERED, not only that the rows are old:
// whatever the log appends next lands at a sequence this node's applier has
// already passed, so it is a record this node will never apply and its own
// resolution can never find — an arbitrated write is reported as a ledger
// contract violation, an additive one as applied, and either way the record is
// on the log for every other node. The remedy is the operator's for the reason
// [foreignStream.err] gives.
func (a aheadOfLog) err(stream string) error {
	return fmt.Errorf("%w: this node's checkpoint on %s is at sequence %d and the "+
		"log ends at %d, so its rows hold records at sequences the log has never "+
		"issued — the stream was rebuilt under it, or the broker was restored from "+
		"a copy older than those rows, which keeps the stream's creation instant — "+
		"and whatever the log appends next lands at a sequence this node has "+
		"already passed and will never apply; neither a read nor a write can be "+
		"answered from these rows, and an operator re-anchors them with `crewlet "+
		"retention reanchor -stream %s`",
		ErrAheadOfLog, stream, a.at.Seq, a.last, stream)
}

// RecordedIdentity reads the creation instant this node last recorded for a
// stream, from its OWN estate.
//
// # Why the node estate and not the replicated one
//
// This is a per-node observation, not shared state: two nodes can legitimately
// have seen a stream at different moments, and a donated snapshot must not
// carry the donor's answer. The framework's own checkpoint table is in the
// replicated estate and is walked by the backup manifest and the adoption
// path, both of which read every row as a DOMAIN — so a row for a stream that
// is not a domain would put a phantom domain in an artefact's manifest, and a
// recipient refuses an artefact naming a domain it does not register.
func RecordedIdentity(ctx context.Context, db *store.DB, stream string) (
	time.Time, bool, error) {

	var created int64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT created_at FROM stream_identity WHERE stream = ?`,
			stream).Scan(&created)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf(
			"statelog: read %s's recorded identity: %w", stream, err)
	}
	return store.DecodeTime(created), true, nil
}

// RecordIdentity writes what this node has seen.
//
// LAST WRITER WINS, deliberately: a node that has decided to follow a
// recreated stream records the new instant, and the next boot then reads it as
// the same stream. The decision to follow one is the OPERATOR's — a reanchor —
// and this is the record of it rather than a second opinion about it.
func RecordIdentity(ctx context.Context, db *store.DB, stream string,
	created, now time.Time) error {

	if created.IsZero() {
		return nil
	}
	return db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO stream_identity (stream, created_at, seen_at)
			VALUES (?, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET
				created_at = excluded.created_at, seen_at = excluded.seen_at`,
			stream, store.EncodeTime(created), store.EncodeTime(now))
		if err != nil {
			return fmt.Errorf("statelog: record %s's identity: %w", stream, err)
		}
		return nil
	})
}
