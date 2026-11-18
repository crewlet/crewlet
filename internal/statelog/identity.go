package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/store"
)

// CONTRACT 1: A STREAM'S IDENTITY, and what a consumer must do when it moves.
//
// A stream deleted and remade restarts its sequences at 1 and is EMPTY: every
// position a node holds names a space that no longer exists, and a consumer
// reading it sees nothing and reports nothing. Only the creation instant
// detects it: a recreated stream's sequences are plausible, and an empty stream
// and an emptied one have one count. WHICH instant, not simply the broker's,
// is [StreamCreatedAt]'s to say ([CreatedAtKey]).
//
// One named contract, because every consumer of a durable stream has the
// hazard: [Runner] answers with a generation, a compacted changelog (no
// checkpoint, no generation) refuses the work; both need one comparison.

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

	// StreamUnknown is a creation instant that could not be established: the
	// broker did not answer, or answered a zero instant. Read as same, a node
	// resumes against a stream it cannot identify; read as recreated, it
	// tears down a healthy node over a broker blip.
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

// CreatedAtKey is the stream METADATA key the create that made a stream stamps
// its creation instant under, the one [StreamCreatedAt] answers (ADR-0083).
//
// Not the broker's instant: a snapshot restore (`nats stream restore`, or a
// `crewlet backup` restore) gives back every record and consumer position but
// dates the stream to the RESTORE, so every restored log would read as
// recreated. A restore keeps the configuration, so the stamp rides there
// ([StampCreatedAt]); a restored stream is judged by where its log ends
// ([ErrAheadOfLog]) and its record at the checkpoint ([ErrLogDiverged]), and
// one the engine provisions after a loss is stamped anew. A stream an identity
// check reads must be stamped by its creator, or its restore is a recreation
// again; an operator copying a stamp onto another stream declares one history.
const CreatedAtKey = "crewlet.created_at"

// StampCreatedAt returns the metadata of a stream about to be created, with
// its creation instant at stamped under [CreatedAtKey].
//
// A NEW MAP: a stream specification is often a value several creates share.
func StampCreatedAt(metadata map[string]string, at time.Time) map[string]string {
	out := make(map[string]string, len(metadata)+1)
	maps.Copy(out, metadata)
	out[CreatedAtKey] = at.UTC().Format(time.RFC3339Nano)
	return out
}

// StreamCreatedAt is the creation instant a stream's identity is keyed to: the
// instant stamped into its configuration when it was created ([CreatedAtKey]),
// and the broker's own for a stream that carries none.
//
// THE ONE READING for every consumer that compares an instant (the applier and
// its log's reads, the memory changelog, the backup), held by
// TestOnlyStreamCreatedAtReadsAStreamsCreationInstant: two readings for one
// stream is one of them calling a restored stream recreated.
//
// A STAMP THAT DOES NOT PARSE IS AN ERROR, never the broker's instant instead:
// unknown is not same ([StreamUnknown]). It names the key and the value, since
// the remedy is the stream's configuration.
func StreamCreatedAt(info *jetstream.StreamInfo) (time.Time, error) {
	if info == nil {
		return time.Time{}, errors.New("statelog: the broker answered no stream " +
			"information to read a creation instant from")
	}
	stamp, stamped := info.Config.Metadata[CreatedAtKey]
	if !stamped {
		return info.Created.UTC(), nil
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, fmt.Errorf("statelog: stream %s carries %s %q, which is not "+
			"an RFC 3339 instant, so it cannot be told from a recreated stream — set it "+
			"to the instant the stream was created at, as the engine stamps it: %w",
			info.Config.Name, CreatedAtKey, stamp, err)
	}
	return at.UTC(), nil
}

// identityResolution is the precision at which two observations of one instant
// are the same observation.
//
// A MICROSECOND, what [store.EncodeTime] keeps, while the broker reports
// nanoseconds: compared exactly, every boot after the first reads a recreation.
const identityResolution = time.Microsecond

// AppliedInstant is the broker's instant for a record as every applier is
// handed it ([Record.StoredAt], [ApplyOptions.StoredAt]): the store's encoding
// of it, UTC at a microsecond, read back through the store's decode.
//
// ONE VALUE ON EVERY NODE AND EVERY PATH, which the broker's own is not: the
// client reports nanoseconds in the node's zone, a deferred record carries the
// deferred table's encoding, and a document encodes what it is handed, so two
// nodes would hold different bytes for one record, the difference
// [Domain.ClaimsIdentity] asserts there is none of. Exported for a harness
// that calls a domain's applier by hand, which must hand it this, as the loop
// does. The store's helpers, never a copy: the deferred table is written
// through them, and a second spelling would drift from it.
func AppliedInstant(t time.Time) time.Time { return store.DecodeTime(store.EncodeTime(t)) }

// IdentityOf compares a stream's current creation instant against the one this
// node recorded.
//
// A PURE FUNCTION, so every consumer reaches one verdict, testable without a
// broker.
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
// The caller's response is never a retry: what it derived from that stream is
// stale in a way no re-read repairs, so it refuses the work rather than serve
// an empty answer.
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
// THE REMEDY IS THE OPERATOR'S, never a retry: a re-read reads the same
// mismatch, and following the rebuilt stream declares lost whatever the old
// one held that this node never applied, which no node may decide alone.
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
// cannot show it: a broker restored from a copy older than this node's rows
// keeps its stream, instant and all, and only where the log ENDS differs. Both
// refuse `wrong_stream` with one remedy; the sentinel tells them apart.
var ErrAheadOfLog = errors.New("statelog: this node's checkpoint is past the log's end")

// ErrGenerationPassed reports a domain a peer has re-anchored while this node's
// rows stayed in the generation before: the fleet's history now continues from
// the re-anchored peer's rows, which this node's are not.
//
// ITS OWN SENTINEL BESIDE [ErrStreamRecreated] and [ErrAheadOfLog]: a node
// whose checkpoint was below a restored end sees neither, its log looks like
// its own history, and its rows miss everything the re-anchoring peer held
// past the copy. The history exists on that peer, so the node usually ADOPTS
// its snapshot on its own, through the ordinary join; the words name no peer
// because a generation only an evicted node opened has none to adopt, and one
// this node's failed reanchor opened is finished by running it again
// ([passedGeneration.err] names which).
var ErrGenerationPassed = errors.New("statelog: the log moved past this node's generation")

// ErrLogDiverged reports a log whose record at this node's checkpoint is not the
// record this node consumed there: the log continues a history these rows are
// not.
//
// ITS OWN SENTINEL BESIDE [ErrAheadOfLog], whose finding lasts only while the
// restored log ENDS below the checkpoint; once anything writes past it, what
// still separates the histories is the record at the checkpoint's sequence,
// which on one stream never changes ([Runner.VerifyCheckpoint]). A member not
// caught up answers that it has no record, never another one, so the finding
// is STICKY: nothing on the log brings these rows level, and applying past the
// checkpoint would put a second history on top of them.
var ErrLogDiverged = errors.New("statelog: the log's record at this node's checkpoint is not the one it consumed")

// divergedLog is what established that a log diverged from an applier's rows:
// the checkpoint, the broker instant of the record the checkpoint names, and
// the instant of the record the log holds at the same sequence. The consumed
// instant is never zero: a checkpoint that names no record is compared with
// nothing ([Runner.VerifyCheckpoint]).
type divergedLog struct {
	at             Position
	consumed, held time.Time
}

// err is the one sentence every refusal over a diverged log carries — the
// applier's stop, a read's `wrong_stream` and a write's.
func (d divergedLog) err(stream string) error {
	// "AT", NEVER "PAST": a log written exactly to the checkpoint's sequence
	// holds the other history's record there and nothing past it.
	return fmt.Errorf("%w: this node's checkpoint on %s is at sequence %d, on the "+
		"record the broker stored at %s, and the log's record at %d was stored at "+
		"%s — the broker was restored from a copy older than these rows and another "+
		"record has since been written at that sequence, so the log continues a "+
		"history they are not; nothing past the checkpoint is applied, neither a "+
		"read nor a write can be answered from these rows, and an operator "+
		"re-anchors them with `crewlet retention reanchor -stream %s` (the restored "+
		"case) or replaces them with a peer's",
		ErrLogDiverged, stream, d.at.Seq, d.consumed.UTC().Format(time.RFC3339Nano),
		d.at.Seq, d.held.UTC().Format(time.RFC3339Nano), stream)
}

// ErrLogTruncated reports a log that has lost records a PEER's rows hold: a
// node on the same stream, in the same generation and not evicted, stands past
// the log's end, or says the log diverged from its rows.
//
// A finding about the FLEET, not this node: its rows are the log's own
// history, so its reads are served, but its writes wait for the operator's
// choice. If the peer's rows are kept (a restored reanchor), a write from here
// applies nowhere after it was acknowledged. An eviction or a readmission is
// the exception ([Request.NodeGate]): evicting the peer is one of the choices.
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

// HoldsRecord reports whether log holds, at seq, the record the broker stored at
// storedAt — a checkpoint's record, named by its sequence and its instant.
//
// FALSE for everything else: a sequence the log no longer holds or never
// reached, another record there, a zero instant. A caller weighs every false
// as history the log may have lost.
func HoldsRecord(ctx context.Context, log LogReader, seq uint64, storedAt time.Time) (bool, error) {
	if seq == 0 || storedAt.IsZero() {
		return false, nil
	}
	_, _, held, ok, err := log.At(ctx, seq)
	if err != nil {
		return false, fmt.Errorf("statelog: read the record at sequence %d: %w", seq, err)
	}
	return ok && sameRecord(held, storedAt), nil
}

// sameRecord reports whether two broker instants name one record at one
// sequence — at the resolution a checkpoint row keeps, for the reason
// [identityResolution] gives.
func sameRecord(a, b time.Time) bool {
	return a.Truncate(identityResolution).Equal(b.Truncate(identityResolution))
}

// passedGeneration is what established that the log moved past this applier's
// rows' generation: the checkpoint the rows stand at, the generation the log is
// on, the node whose record showed it (empty where the positions register did),
// and who holds that generation — which is what decides the remedy.
type passedGeneration struct {
	at     Position
	fleet  uint32
	writer string
	holder passedHolder
}

// passedHolder is who holds the generation a log moved to past this node's
// rows, as far as this node can tell: the fact the remedy turns on.
type passedHolder int

const (
	// passedByPeer: a live peer holds the generation, or nothing yet says
	// none does. This node adopts its snapshot on its own.
	passedByPeer passedHolder = iota

	// passedByEvicted: only evicted nodes ever held it, so there is no
	// snapshot to adopt; the remedy is this node's own reanchor, which voids
	// that generation.
	passedByEvicted

	// passedByOwnAttempt: the record opening it is this node's own, from a
	// reanchor that did not complete; running it again completes it.
	passedByOwnAttempt
)

// err is the one sentence every refusal over a passed generation carries — the
// applier's stop, a read's `wrong_stream` and a write's. THE REMEDY FOLLOWS
// [passedHolder]: promising adoption where no peer holds the generation leaves
// a node waiting for ever on a repair that cannot come.
func (p passedGeneration) err(domain, stream string) error {
	lead := fmt.Sprintf("%s's rows are at generation %d of %s", domain, p.at.Generation, stream)
	who := "a peer"
	if p.writer != "" {
		who = "peer " + p.writer
	}
	switch p.holder {
	case passedByOwnAttempt:
		return fmt.Errorf("%w: %s and the log carries this node's own record opening "+
			"generation %d, appended by a reanchor of it that did not complete — the "+
			"log continues in that generation while these rows' checkpoint is still in "+
			"the one before, so neither a read nor a write can be answered from them "+
			"until an operator re-runs the reanchor (`crewlet retention reanchor "+
			"-stream %s`), which completes it from that record",
			ErrGenerationPassed, lead, p.fleet, stream)
	case passedByEvicted:
		return fmt.Errorf("%w: %s and the log continues in generation %d, which %s "+
			"opened and only nodes the fleet has evicted hold — there is no snapshot "+
			"at that generation to adopt, so neither a read nor a write can be answered "+
			"from these rows until an operator re-anchors this stream (`crewlet "+
			"retention reanchor -stream %s`), which follows it from this node's own "+
			"checkpoint with that generation void",
			ErrGenerationPassed, lead, p.fleet, who, stream)
	}
	return fmt.Errorf("%w: %s and %s has re-anchored it to generation %d, so the "+
		"history the log now continues is that peer's rows rather than these — "+
		"neither a read nor a write can be answered from them, and nothing on the log "+
		"can bring them level; this node adopts a snapshot from a peer at generation "+
		"%d, which it asks the fleet for on its own — and if no live node holds that "+
		"generation because the peer that opened it is gone for good, an operator "+
		"evicts that peer (`crewlet retention evict`) and re-anchors this stream "+
		"(`crewlet retention reanchor -stream %s`)",
		ErrGenerationPassed, lead, who, p.fleet, p.fleet, stream)
}

// pastEnd reports a checkpoint past a log's last sequence — [Health.AheadOfLog],
// the zero fence's own reading and [Runner.ObserveEnd] ask it in one spelling.
//
// ONE PREDICATE, so the read path, the write path and their verdict cannot
// drift on the boundary. A checkpoint EQUAL to the end is not past it: it is
// every caught-up node.
func pastEnd(checkpoint, last uint64) bool { return checkpoint > last }

// aheadOfLog is one reading that found an applier's checkpoint past its log's
// end: the checkpoint the reading was paired with, and the end it reported.
type aheadOfLog struct {
	at   Position
	last uint64
}

// err is the one sentence every refusal over a checkpoint past the log's end
// carries, a write's at fence 0 and at the zero fence alike. It names why no
// write can be answered: the log's next append lands at a sequence this node's
// applier has passed, a record it never applies nor resolves (an arbitrated
// write would read as a ledger contract violation, an additive one as
// applied), yet one on the log for every other node. The remedy is the
// operator's ([foreignStream.err]).
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
// A per-node observation: two nodes may have seen a stream at different
// moments, and a donated snapshot must not carry the donor's answer. Not the
// replicated checkpoint table either: the backup manifest and adoption read
// its every row as a DOMAIN, and a recipient refuses an artefact naming a
// domain it does not register.
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
// LAST WRITER WINS: it records the operator's decision (a reanchor) to follow
// a recreated stream, so the next boot reads it as the same stream.
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
