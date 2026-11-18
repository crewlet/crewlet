package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

// Domain is one replicated state machine: one ordered stream, one
// deterministic applier, and N identical copies in N node databases.
//
// The framework declares it because the framework is its consumer: it calls
// the applier, derives the scrub list, picks the replay loop and gates seat
// admission from these answers. Domain packages export concrete types.
type Domain interface {
	// Name is the register key, the manifest key and the operator
	// column. NEVER renamed: it is durable in all three, independently.
	Name() string

	// Stream is the stream this domain's records live on, and the replay
	// protocol its own shape implies.
	Stream() StreamSpec

	// RecordVersion is the highest record version THIS BUILD can decode;
	// a record above it is retained, not skipped (package doc, "A record
	// this build cannot decode is RETAINED, with one exception").
	RecordVersion() int

	// Envelope decodes the always-readable half of a record.
	//
	// It MUST NOT FAIL ON AN UNKNOWN VERSION: without an envelope, a record
	// this build cannot decode has no position, kind, subject or scope to
	// be indexed, probed for or reported on, and could only be dropped. Its
	// Scope is NEVER empty: an empty scope reads as "touches nothing" and
	// would license every read the record makes stale.
	Envelope(payload []byte) (Envelope, error)

	// InstallsGate reports whether this record installs an APPLY GATE: a
	// rule under which a durable record produces no rows on ANY node.
	//
	// Answered from the envelope alone: the only caller is the
	// unknown-version arm, which cannot decode the payload. An undecodable
	// record that may be a gate is a STOP, not a deferral (package doc, "A
	// record this build cannot decode is RETAINED, with one exception").
	InstallsGate(env Envelope) bool

	// NodeGate reports whether this record EVICTS OR READMITS A NODE: the
	// one record a write flagged [Request.NodeGate] may carry.
	//
	// Narrower than [Domain.InstallsGate] on purpose: a purge installs a
	// gate but is an ordinary write. The flag excuses three fences (the
	// passed generation, a peer's truncated rows, the gate reserve), so a
	// purge flagged by mistake under the wider answer would pass them all
	// and spend the room kept for the eviction that unpins a full log.
	//
	// Answered from the envelope alone, as InstallsGate is. statelogtest
	// certifies that only the domain's own eviction and readmission (if
	// any) answer true, and both install a gate.
	NodeGate(env Envelope) bool

	// Tables is every table this domain writes, with the class that says
	// what a snapshot, an identity claim and a local sweep each do with
	// it. A table without a class fails a static test rather than
	// silently joining whichever behaviour its absence produced.
	Tables() map[string]TableClass

	// DeferredTable is where a record this build cannot decode is
	// retained, byte for byte, at its original position. Classed Local;
	// the domain names it because the domain owns the schema and the
	// framework writes the statements.
	DeferredTable() string

	// ScopeIndex is the table the framework probes for deferred records
	// whose declared scope intersects a read's or a write's closure.
	// Classed Local, written in its deferred row's transaction, and opaque
	// to the framework, which takes a ScopeSet and a table name, never a
	// domain's own terms.
	ScopeIndex() string

	// OpsTable is the idempotency ledger: the op ids this node's applier
	// has written, at the positions it wrote them.
	//
	// [Divergent] so it TRAVELS with its rows: an adopter answers a retry
	// from the donor's entries. As [Local] it would be scrubbed, and the
	// adopter would answer `unknown` for every op minted before the
	// adoption. Not Replicated only because `applied_at` is this node's
	// clock, read by its own sweep alone.
	//
	// EMPTY only for a domain whose apply is total under a monotone version
	// guard AND whose writer never reports a committed position to a
	// caller; asserted, since a writer that answers must say if it landed.
	OpsTable() string

	// GatesSeatAdmission reports whether a SEAT depends on this domain's
	// copy: a node admits a seat only once the copy is established at lag
	// zero ([Health.Established], strict), and the copy going out of
	// service routes the node's seats through peers, so a node that can
	// name none gives them up. It does NOT decide whether the copy is
	// judged: every copy is ([Health.Healthy], ADR-0079).
	//
	// TRUE for a domain a seat's tools read every turn, where a lagging
	// copy denies work the seat was just handed. FALSE for a compacted
	// domain, whose gap is coverage, not a fault (statelogtest refuses
	// true there), and for a busy log a seat rarely reads, whose lag would
	// withhold every seat claim on the node.
	GatesSeatAdmission() bool

	// ClaimsIdentity reports whether two nodes at one checkpoint are
	// ASSERTED to hold byte-identical Replicated tables. False for a
	// compacted domain, whose per-node coverage legitimately differs.
	//
	// The framework's half: what it hands an applier OF THE RECORD is one
	// value on every node however the record arrived: payload, position,
	// and [Record.StoredAt] in the store's encoding. The rest of
	// [ApplyOptions] (Now, Epoch) is this node's, per batch, and may
	// decide only a [Divergent] row. The domain's half: no Replicated byte
	// depends on anything but the record, its rows and the declarations.
	ClaimsIdentity() bool

	// FeedGroup is the fleet-wide consumer this domain's WAKE FEED runs as
	// on its own stream, or empty for a domain with no wake feed.
	//
	// Declared because the feed advancing it and the trim's
	// `feed_ack_floor` term waiting for it (on the trim duty's node)
	// cannot ask each other. EMPTY makes the term ABSENT, not zero. Wrong
	// answers are quiet: empty with a feed trims records nobody was woken
	// for; another domain's group blocks the trim on a consumer nobody opens.
	//
	// NEVER RENAMED: the name is the fleet's position on its log. A new
	// name replays every retained record as a wake (only the change feed's
	// claim and wake-id dedupe hold some back) and stalls the trim behind it.
	FeedGroup() string
}

// ReplayProtocol is how a domain's stream behaves under replay; the
// framework picks a loop from it and never infers or defaults one.
//
// A strict loop over a compacted stream stalls for ever on the first hole; a
// compacted loop over a strict stream silently accepts a gap that is data
// loss. Neither loop can tell, so the domain that configured the stream says.
type ReplayProtocol string

const (
	// ReplayStrict is a log: every sequence between the floor and the
	// head exists, so the loop may treat contiguity as an invariant and a
	// hole as a fault.
	ReplayStrict ReplayProtocol = "strict"

	// ReplayCompacted is a keyed table: the stream keeps one message per
	// subject, so an ordinary write leaves an interior hole and the loop
	// must step over it. A gap here is a coverage number rather than a
	// fault.
	ReplayCompacted ReplayProtocol = "compacted"
)

// ReplayProtocols is every protocol, for the enum's own validation and for a
// test that walks them all.
var ReplayProtocols = []ReplayProtocol{ReplayStrict, ReplayCompacted}

// Valid reports whether a protocol off the wire or out of a config is one
// this build knows.
func (r ReplayProtocol) Valid() bool {
	for _, known := range ReplayProtocols {
		if r == known {
			return true
		}
	}
	return false
}

// StreamSpec is a domain's stream: what the broker is asked for, plus the
// replay protocol, which the client models as a consumer policy and so no
// stream setting can carry.
type StreamSpec struct {
	// Name is the stream. Conventionally CREWLET_<DOMAIN>_LOG.
	Name string

	// Subjects is the subject space this domain publishes into, as the
	// broker is told it: normally one wildcard.
	Subjects []string

	// ArbitratedKinds are the subject kinds whose writes carry a
	// per-subject expectation, and so the only kinds an anchor is written
	// for.
	//
	// Per kind, not per stream: one log carries arbitrating kinds, additive
	// kinds and the framework's barrier. An anchor for the barrier, whose
	// writes all share ONE subject, would put an upsert per read on one hot
	// row inside the store's only writer, and break the property that a
	// barrier writes zero rows and never enters the applier's row budget.
	//
	// EMPTY means a domain that publishes no expectation; its row guard is
	// its idempotency.
	ArbitratedKinds []string

	// SubjectPrefix is what a subject's own path is appended to. Declared,
	// not derived from Subjects: stripping a wildcard is a parser, and a
	// domain with two subject spaces or no trailing wildcard would
	// silently publish to a subject no consumer covers.
	SubjectPrefix string

	// MaxBytes is the domain's WHOLE byte budget. Crossing it REFUSES an
	// append rather than dropping the oldest record, so zero (unlimited)
	// fills the volume. It is the one setting a node may replace, with its
	// own Tier A sizing ([StreamSpec.Instantiates]).
	MaxBytes int64

	// MaxRecordBytes is the largest payload this domain's decide forms,
	// before the signature, and it is a LIMIT: the publisher refuses a
	// larger record `record_too_large` before sending, and the broker
	// refuses a message above [StreamSpec.MaxAppendBytes], which holds a
	// peer on another build to it too.
	//
	// Per domain because it sizes the gate reserve ([Reservation]): past
	// the soft ceiling every peer may hold one append of this size in
	// flight unseen; one transport-wide size made it most of a small log.
	// REQUIRED, and at most [MaxTransportRecordBytes]: the transport's
	// maximum less what a stored record carries beside its payload.
	MaxRecordBytes int64

	// MaxPerSubject retains only the newest message per subject, turning
	// the stream from a log into a keyed table. Zero is a log; 1 is the
	// compacted shape, and the pairing with ReplayCompacted is asserted.
	MaxPerSubject int

	// MaxAge bounds how long a message is kept. Zero for a log (an age
	// bound on a log deletes state a node has not applied); non-zero only
	// on a compacted stream whose rows are durable in SQL anyway.
	MaxAge time.Duration

	// Duplicates is the window a repeated op id is collapsed in. It must
	// outlast a publisher's whole retry budget, and is an optimisation,
	// not a mechanism (package doc, "Retry is three layers and only one
	// may be relied on").
	Duplicates time.Duration

	// Replay is the protocol, declared rather than inferred.
	Replay ReplayProtocol
}

// Validate refuses a spec whose settings and protocol disagree: neither
// replay loop can detect its own mismatch ([ReplayProtocol]), so the
// declaration is checked here, once.
func (s StreamSpec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("statelog: stream spec has no name")
	}
	if len(s.Subjects) == 0 {
		return fmt.Errorf("statelog: stream %q claims no subjects", s.Name)
	}
	if s.SubjectPrefix == "" {
		return fmt.Errorf("statelog: stream %q declares no subject prefix — it "+
			"is what every record's own path is appended to, and guessing it "+
			"from the wildcard would publish to a subject nothing consumes",
			s.Name)
	}
	if !s.Replay.Valid() {
		return fmt.Errorf("statelog: stream %q declares replay %q (want %s or %s)",
			s.Name, s.Replay, ReplayStrict, ReplayCompacted)
	}
	switch s.Replay {
	case ReplayStrict:
		if s.MaxPerSubject != 0 {
			return fmt.Errorf("statelog: stream %q is %s but keeps %d "+
				"message(s) per subject — a per-subject limit removes an "+
				"interior sequence, which is the one thing a strict loop "+
				"treats as a fault", s.Name, ReplayStrict, s.MaxPerSubject)
		}
		if s.MaxAge != 0 {
			return fmt.Errorf("statelog: stream %q is %s and sets max_age %s "+
				"— an age bound on a log deletes records a node has not "+
				"applied, and there is no node whose word it takes",
				s.Name, ReplayStrict, s.MaxAge)
		}
	case ReplayCompacted:
		if s.MaxPerSubject != 1 {
			return fmt.Errorf("statelog: stream %q is %s but keeps %d "+
				"message(s) per subject — a compacted domain's stream is a "+
				"keyed table and keeps exactly one", s.Name, ReplayCompacted,
				s.MaxPerSubject)
		}
	}
	if s.MaxBytes <= 0 {
		return fmt.Errorf("statelog: stream %q has no byte ceiling — a log "+
			"with none fills the volume its own applier commits to", s.Name)
	}
	if s.MaxRecordBytes <= 0 || s.MaxRecordBytes > MaxTransportRecordBytes {
		return fmt.Errorf("statelog: stream %q declares a largest record of %d "+
			"bytes (want 1..%d, the largest the transport carries once a "+
			"record is signed and sent) — it is what the log's gate reserve is "+
			"sized by and the one refusal a record meets before it is sent, so "+
			"a log without one keeps a reserve sized for nothing and a log "+
			"past it admits records the broker refuses", s.Name,
			s.MaxRecordBytes, MaxTransportRecordBytes)
	}
	if s.Duplicates <= 0 {
		return fmt.Errorf("statelog: stream %q sets no duplicate window", s.Name)
	}
	return nil
}

// Instantiates refuses a spec that is not domain d's stream: every setting but
// the byte ceiling must be the domain's own, names included.
//
// The NAMES because a node keys per-log records by stream (checkpoint,
// anchors) or by domain name (position row, floor), so a mismatch files one
// log's positions under another's name. The REST because the replay loop,
// the arbitration and the duplicate window are chosen from the domain's
// declaration, and silently wrong for a stream of another shape. Only
// MaxBytes is the node's, sized by its Tier A; the largest record is not,
// since it sizes the gate reserve and `max_msg_size` holds peers to it, so a
// node's own choice would admit records its peers do not.
func (s StreamSpec) Instantiates(d Domain) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("statelog: stream %q names no domain", s.Name)
	}
	want := d.Stream()
	switch {
	case s.Name != want.Name,
		s.SubjectPrefix != want.SubjectPrefix,
		!slices.Equal(s.Subjects, want.Subjects):
		return fmt.Errorf("statelog: stream (%q, %q, %s) is not the %s domain's, "+
			"which is (%q, %q, %s)", s.Name, s.SubjectPrefix, words.And(s.Subjects), d.Name(),
			want.Name, want.SubjectPrefix, words.And(want.Subjects))
	case s.Replay != want.Replay,
		s.MaxPerSubject != want.MaxPerSubject,
		s.MaxAge != want.MaxAge,
		s.MaxRecordBytes != want.MaxRecordBytes,
		s.Duplicates != want.Duplicates,
		!slices.Equal(s.ArbitratedKinds, want.ArbitratedKinds):
		return fmt.Errorf("statelog: stream %q is not the %s domain's: "+
			"its replay, per-subject bound, age bound, largest record, duplicate "+
			"window or arbitrated kinds differ from the domain's own declaration, "+
			"and every one of them decides how the framework reads and writes "+
			"that log", s.Name, d.Name())
	}
	return nil
}

// TableClass says what a snapshot, an identity claim and a local sweep each
// do with one of a domain's tables.
//
// Every such list (what a donated snapshot scrubs, what nodes are asserted to
// agree on, what a node sweeps) derives from [Domain.Tables], never a
// hardcoded list, which "would silently omit whatever a deployment actually
// has" ([internal/backup]); a table without a class fails a static test.
type TableClass int

const (
	// Replicated is written ONLY by applying a committed record of this
	// domain, by a function of (rows, record, options) alone. It is IN
	// the identity claim, it travels inside a snapshot, and nothing
	// sweeps it locally.
	Replicated TableClass = iota

	// Divergent is written only by applying a committed record and
	// TRAVELS inside a snapshot, but is outside the identity claim: its
	// content depends on a per-node or per-epoch value the framework
	// passes in. It keeps the claim exactly "Replicated"; classed Local
	// instead, the table would be scrubbed and an adopter would hold a
	// tail where its peers hold a year ([Domain.OpsTable]).
	Divergent

	// Local is this node's own rows. SCRUBBED from every snapshot,
	// excluded from the claim, swept on this node's own schedule.
	Local

	// Derived is rebuildable from Replicated tables IN THE SAME FILE with
	// no network. It travels or not at the domain's choice and is never
	// in the claim.
	Derived
)

// String names a class for an operator surface and for the static test's
// failure message.
func (c TableClass) String() string {
	switch c {
	case Replicated:
		return "replicated"
	case Divergent:
		return "divergent"
	case Local:
		return "local"
	case Derived:
		return "derived"
	}
	return fmt.Sprintf("TableClass(%d)", int(c))
}

// Valid reports whether a class is one of the four.
func (c TableClass) Valid() bool { return c >= Replicated && c <= Derived }

// Subject names the OBJECT a record arbitrates over, in the domain's own
// vocabulary rather than as a wire string.
//
// ONE SUBJECT PER OBJECT: the broker's per-subject expectation is the whole
// concurrency control, so objects sharing a subject serialize against each
// other and an object spread across two arbitrates against neither.
type Subject struct {
	// Kind is the object kind — the domain's own enum, as a string.
	Kind string

	// ID is the object's identity within its kind. EMPTY is legal and
	// means a kind with exactly one object, which the barrier is.
	ID string
}

// String renders a subject for a log line and for a scope path.
func (s Subject) String() string {
	if s.ID == "" {
		return s.Kind
	}
	return s.Kind + "." + s.ID
}

// ScopeSet is the set of objects an operation is ABOUT: qualified paths under
// a containment order the domain defines, opaque to the framework, which only
// asks the domain's own index whether two intersect. That is what lets the
// read levels and the write path's deferral probe predate any domain.
type ScopeSet struct {
	// Paths are the qualified paths this operation touches.
	Paths []string
}

// Empty reports a scope that names nothing — which is never a legal record
// scope and always a bug at the caller, so the framework refuses rather than
// treating it as "touches everything" or as "touches nothing".
func (s ScopeSet) Empty() bool { return len(s.Paths) == 0 }

// Envelope is the always-decodable half of a record.
//
// EVERY FIELD IS READABLE BY EVERY BUILD, FOR EVER: a record a build cannot
// decode is retained and indexed by these fields, so a field added later is
// readable by every build except the one that needs it.
type Envelope struct {
	// V is the record version. A record above this build's
	// RecordVersion is retained rather than skipped.
	V int

	// Kind is the record kind within the domain.
	Kind string

	// Op is the operation within that kind, for whether this record
	// INSTALLS A GATE: a gate need not be a kind of its own (a purge is a
	// destructive operation on an ordinary task subject), and a node that
	// cannot decode the payload must still answer. Only
	// [Domain.InstallsGate] reads it.
	Op string

	// Subject is the object this record arbitrates over.
	Subject Subject

	// OpID is the caller-visible operation this record belongs to,
	// stable across every round and every retry.
	OpID string

	// Gen is the generation the writer published in. A record from a
	// previous generation is comparable and safely stale rather than
	// indistinguishable from a current one.
	Gen uint32

	// Scope is every object this record's rows touch, more than its
	// subject whenever an operation rewrites a neighbour. NEVER empty:
	// that would claim the record makes nothing stale, which a record a
	// build cannot read may not claim.
	Scope ScopeSet

	// Writer is the node that published the record, which the eviction
	// gate reads and nothing else does.
	Writer string
}

// Record is one committed record: its envelope, its position, and the bytes
// the domain decodes.
type Record struct {
	Envelope
	// Position is where this record sits, composed by the framework from
	// the broker's sequence and the writer's own generation.
	Position Position

	// Payload is the record's BODY, with the framework's signature frame
	// already opened and checked (ADR-0031), so no applier unwraps for
	// itself and none that forgot can fail on every record.
	//
	// A retained record is stored FRAMED instead (framed), because
	// lossless means the bytes: the build that can finally read it
	// re-opens and re-verifies them rather than trusting an earlier
	// build's look.
	Payload []byte

	// framed is the record exactly as published, frame and all: what the
	// deferred table stores and [Runner] re-verifies on a reprocess.
	// Unexported so no applier can decode bytes the framework has not
	// authenticated.
	framed []byte

	// StoredAt is the broker's own timestamp, in the store's encoding (UTC
	// at a microsecond, [AppliedInstant]) whether the record applied on
	// arrival or after being held back, so it is ONE VALUE on every node,
	// not merely one instant.
	StoredAt time.Time

	// verdict is the framework's signature-check conclusion, unexported
	// so only the apply loop acts on it: rows that depended on which keys
	// a node held would not be a pure function of the record.
	verdict recordseal.Verdict

	// ack acknowledges delivery. Unexported: only the apply loop calls
	// it, after the consuming transaction commits, since an ack inside
	// that transaction would outlive a rollback and re-run.
	ack func() error

	// release hands the delivery back for immediate redelivery
	// ([Message.Release]); nil where the fetcher has none. Unexported:
	// only the apply loop calls it, outside a transaction ([Runner.Run]'s
	// unwind).
	release func() error
}

// Applier is a domain's deterministic state machine. ONE per domain, ONE
// writer.
//
//   - APPLY IS IDEMPOTENT at a position.
//   - APPLY IS A PURE FUNCTION of (the rows in this transaction, the record,
//     the options): no clock, config or coordination read, node identity,
//     randomness, or collection written from an unsorted map.
//
// Purity is the identity claim: anything read from the world is a value two
// nodes can differ on, so what an applier needs arrives through
// [ApplyOptions]. No Order (the order is total) and no Reset (the checkpoint
// commits with the rows).
type Applier interface {
	// Apply writes this record's rows and reports how many. The framework
	// commits the rows, op id, anchor and checkpoint in this same
	// transaction.
	//
	// The row count feeds the budget (a transaction ends at the first
	// record boundary at or past it), and only the applier knows it: a
	// wrong count holds the store's only writer for as long as it is wrong.
	Apply(ctx context.Context, tx *sql.Tx, rec Record, opts ApplyOptions) (rows int, err error)

	// Gated reports whether this record must produce NO rows, read from
	// this same transaction: a gate (a permanent deletion marker on its
	// subject, an eviction of its writer) lives in the domain's rows and
	// must come from the state the record would apply against.
	Gated(ctx context.Context, tx *sql.Tx, rec Record) (Reason, bool, error)

	// Committed runs AFTER the transaction commits, for consequences that
	// are not rows: the store re-runs a transiently failed body, so a side
	// effect in Apply could happen twice.
	Committed(ctx context.Context)
}

// RetentionHook is the optional half of the [Applier] contract: after a
// committed batch that RETAINED at least one record, the framework calls
// Retained with how many, IMMEDIATELY BEFORE [Applier.Committed].
//
// A retained record never reaches Apply, yet every read its scope covers is
// unknown from that commit ([Runner.Deferred]), and only the applier knows
// which listener holds a decision that may no longer stand. A count, since
// what the record is about is in a payload this node cannot read: an applier
// that cannot say whose standing is unknown says everyone's. A record the
// loop's reprocess KEEPS calls nothing; it was announced when first retained.
// Implementers assert `var _ statelog.RetentionHook = (*Applier)(nil)`: a
// misspelt method silently fails the framework's type assertion.
type RetentionHook interface {
	// Retained runs after the commit and before Committed, on the apply
	// loop's own goroutine. It must not block.
	Retained(ctx context.Context, records int)
}

// ApplyOptions carries every value an applier would otherwise read from the
// world. Filled PER BATCH ([Runner.batchOptions]), except StoredAt, which is
// per record and the broker's ([Record.StoredAt]).
type ApplyOptions struct {
	// Now is the batch's instant. NOT for durations: it is the same
	// value for every record in a batch, so a difference between two
	// records' Now values is a batch boundary rather than elapsed time.
	Now time.Time

	// StoredAt is [Record.StoredAt]: UTC at a microsecond on every node
	// and every path a record reaches the applier by.
	StoredAt time.Time

	// ArbitratedKinds is the stream's declaration, carried so an applier
	// reads the framework's static value rather than a second copy.
	ArbitratedKinds []string

	// Epoch is the per-epoch configuration the domain declared it reads,
	// as this node's epoch stood when the BATCH began ([RunnerDeps.Epoch]).
	// Passed in so two nodes briefly on different epochs produce rows a
	// reader can account for (the Divergent class); a node moves to a new
	// epoch at its next batch, not its next restart.
	Epoch map[string]any

	// MaxVariables is the engine's probed bind-parameter limit, which
	// [store.InsertRows] divides by a row's width to size a chunk. Carried
	// here because an applier holds only a transaction; without it each
	// would need a store handle to write child rows as more than one
	// statement per row.
	//
	// ZERO IS UNSET, not a limit: [store.RowsPerInsert] reads it as no room
	// and falls back to one row per statement, never a statement the
	// engine refuses.
	MaxVariables int
}
