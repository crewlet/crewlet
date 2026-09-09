package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Domain is one replicated state machine: one ordered stream, one
// deterministic applier, and N identical copies in N node databases.
//
// # Why the FRAMEWORK declares this and not the domain
//
// CLAUDE.md's rule is that an interface belongs to its consumer, and here the
// framework is the consumer: it calls the applier, derives the scrub list,
// picks the replay loop and gates seat admission from these answers. The
// domain packages export concrete types. This is the same arrangement
// [internal/projection] states at its own Applier — "DECLARED HERE,
// implemented by the packages that own the documents" — one layer down.
type Domain interface {
	// Name is the register key, the manifest key and the operator
	// column. NEVER renamed: it is durable in three places that have no
	// idea about each other.
	Name() string

	// Stream is the stream this domain's records live on, and the replay
	// protocol its own shape implies.
	Stream() StreamSpec

	// RecordVersion is the highest record version THIS BUILD can decode.
	// A record above it is retained rather than skipped — see the
	// package doc's deferral contract.
	RecordVersion() int

	// Envelope decodes the always-readable half of a record.
	//
	// IT MUST NOT FAIL ON AN UNKNOWN VERSION. The deferral contract rests
	// on it: without an envelope there is no position, no kind, no
	// subject and no scope, so a record a build cannot decode could not
	// be indexed, probed for or reported on — only dropped. And its
	// Scope is NEVER empty, because an empty scope is indistinguishable
	// from "touches nothing" and would license every read the record
	// makes stale.
	Envelope(payload []byte) (Envelope, error)

	// InstallsGate reports whether this record installs an APPLY GATE — a
	// rule under which a durable record produces no rows on ANY node.
	//
	// ANSWERED FROM THE ENVELOPE ALONE, because the only caller is the
	// unknown-version arm, which by definition cannot decode the payload.
	// A record this build cannot decode and cannot rule out as a gate is
	// a STOP rather than a deferral: a deferred gate does not postpone
	// one record's effect on one node, it silently licenses every record
	// above it.
	InstallsGate(env Envelope) bool

	// Tables is every table this domain writes, with the class that says
	// what a snapshot, an identity claim and a local sweep each do with
	// it. The framework derives all three from this map, so a table added
	// without a class fails a static test rather than silently joining
	// whichever behaviour its absence happened to produce.
	Tables() map[string]TableClass

	// ScopeIndex is the table the framework probes for deferred records
	// whose declared scope intersects a read's or a write's closure.
	// Classed Local, written in the same transaction as the deferred row
	// it belongs to, and OPAQUE to the framework — which takes a ScopeSet
	// and a table name and never a domain's own terms.
	ScopeIndex() string

	// OpsTable is the idempotency ledger: the op ids this node's applier
	// has written, at the positions it wrote them.
	//
	// EMPTY only for a domain whose apply is a total function under a
	// monotone version guard AND whose writer never reports a committed
	// position to a caller. That pairing is asserted rather than assumed,
	// because half of it is not enough: a domain with a total apply whose
	// writer answers a caller still needs to say whether the answer
	// landed.
	OpsTable() string

	// ReadinessInput reports whether this domain's health gates seat
	// admission. A strictly ordered domain's stall is a fault; a
	// compacted domain's gap is a coverage number, and shedding a
	// company's seats for one would be the outage the number exists to
	// avoid.
	ReadinessInput() bool

	// ClaimsIdentity reports whether two nodes at one checkpoint are
	// ASSERTED to hold byte-identical Replicated tables.
	//
	// False for a compacted domain, whose per-node coverage legitimately
	// differs. Stating it per domain is what keeps the claim exactly
	// "the Replicated tables of a domain that claims identity" rather
	// than "Replicated minus the tables somebody remembered".
	ClaimsIdentity() bool
}

// ReplayProtocol is how a domain's stream behaves under replay, and the
// framework picks a loop from it rather than inferring one.
//
// # Why this is DECLARED and never defaulted
//
// The two loops differ in the one property neither can check for itself:
// whether an interior sequence can go missing. A strict loop over a compacted
// stream stalls for ever on the first hole, and a compacted loop over a
// strict stream silently accepts a gap that means data loss. Both failures
// are quiet, both are permanent, and the stream's own configuration is what
// decides which is which — so the domain that configured the stream is what
// says which loop reads it.
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
// replay protocol the broker has no field for.
//
// THE PROTOCOL IS NOT A STREAM SETTING and this type is where the two meet.
// The client models replay as a CONSUMER policy, so a stream cannot carry it
// and the framework has to hold the pair together — which is also the honest
// place for it, since the protocol is a claim about what the stream's own
// settings imply rather than one of them.
type StreamSpec struct {
	// Name is the stream. Conventionally CREWLET_<DOMAIN>_LOG.
	Name string

	// Subjects is the subject space this domain publishes into, as the
	// broker is told it — normally one wildcard.
	Subjects []string

	// SubjectPrefix is what a subject's own path is appended to, and it
	// is DECLARED rather than derived from Subjects.
	//
	// Deriving it would mean stripping a wildcard token off a configured
	// string, which is a parser between the framework and every record it
	// publishes: a domain that declared two subject spaces, or one
	// without a trailing wildcard, would silently publish to a subject
	// no consumer covers and no test would say so.
	SubjectPrefix string

	// MaxBytes is the ceiling. Crossing it REFUSES an append rather than
	// dropping the oldest record, so zero — unlimited — is a stream that
	// fills the volume instead.
	MaxBytes int64

	// MaxPerSubject retains only the newest message per subject, turning
	// the stream from a log into a keyed table. Zero is a log; 1 is the
	// compacted shape, and the pairing with ReplayCompacted is asserted.
	MaxPerSubject int

	// MaxAge bounds how long a message is kept. Zero for a log — an age
	// bound on a log deletes state a node has not applied — and non-zero
	// only on a compacted stream whose rows are durable in SQL anyway.
	MaxAge time.Duration

	// Duplicates is the window a repeated op id is collapsed in. It must
	// outlast a publisher's whole retry budget, and it is an
	// optimisation rather than a mechanism: see the package doc on why
	// the ops table is the only layer that may be relied on.
	Duplicates time.Duration

	// Replay is the protocol, declared rather than inferred.
	Replay ReplayProtocol
}

// Validate refuses a spec whose settings and protocol disagree.
//
// The pairing is the whole reason the protocol is a field: a compacted stream
// read by a strict loop stalls on its first ordinary write, and a log read by
// a compacted loop accepts a hole that is data loss. Neither loop can detect
// its own mismatch, so the declaration is checked here, once, at the seam.
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
	if s.Duplicates <= 0 {
		return fmt.Errorf("statelog: stream %q sets no duplicate window", s.Name)
	}
	return nil
}

// TableClass says what a snapshot, an identity claim and a local sweep each
// do with one of a domain's tables.
//
// # Why this is a DECLARATION and not a list somewhere
//
// [internal/backup] already names the failure in its own words: a hardcoded
// list "would silently omit whatever a deployment actually has". Every list
// the framework needs — what a donated snapshot scrubs, what two nodes are
// asserted to agree about, what a node sweeps on its own schedule — is
// derived from this one map, so a table added without a class fails a static
// test instead of joining whichever behaviour its absence resembled.
type TableClass int

const (
	// Replicated is written ONLY by applying a committed record of this
	// domain, by a function of (rows, record, options) alone. It is IN
	// the identity claim, it travels inside a snapshot, and nothing
	// sweeps it locally.
	Replicated TableClass = iota

	// Divergent is written only by applying a committed record and
	// TRAVELS inside a snapshot — but is not in the identity claim,
	// because its content depends on a per-node or per-epoch value the
	// framework passes in.
	//
	// It exists so the claim's membership stays exactly "Replicated"
	// rather than "Replicated minus two tables somebody remembered". The
	// alternative is worse than untidy: classing such a table Local would
	// scrub it from every snapshot, and an adopting node would hold a
	// tail where its peers hold a year.
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
// ONE SUBJECT PER OBJECT, which is what makes the subject the arbitration
// unit: the broker's per-subject expectation is the whole concurrency
// control, so an object that shared a subject with another would serialize
// against it and an object spread across two would arbitrate against neither.
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

// ScopeSet is the set of objects an operation is ABOUT, opaque to the
// framework.
//
// The framework takes a scope and a table name from the domain and never a
// domain's own terms, which is what lets the read levels and the write
// path's deferral probe be written before any domain exists. A scope is a set
// of qualified paths under a containment order the domain defines: the
// framework only ever asks whether two of them intersect, and asks the
// domain's own index.
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
// EVERY FIELD HERE IS READABLE BY EVERY BUILD, FOR EVER. That is not a
// convention: a record a build cannot decode is retained and indexed by these
// fields, so a field added at a later version is readable by every build
// except the one that needs it.
type Envelope struct {
	// V is the record version. A record above this build's
	// RecordVersion is retained rather than skipped.
	V int

	// Kind is the record kind within the domain.
	Kind string

	// Subject is the object this record arbitrates over.
	Subject Subject

	// OpID is the caller-visible operation this record belongs to,
	// stable across every round and every retry.
	OpID string

	// Gen is the generation the writer published in. A record from a
	// previous generation is comparable and safely stale rather than
	// indistinguishable from a current one.
	Gen uint32

	// Scope is every object this record's rows touch, which is more than
	// its subject whenever an operation rewrites a neighbour. NEVER
	// empty: an empty scope would say "this record makes nothing stale",
	// which is the one claim a record a build cannot read may not make.
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

	// Payload is the record as published. The framework never decodes it.
	Payload []byte
}

// Applier is a domain's deterministic state machine. ONE per domain, ONE
// writer.
//
//   - APPLY IS IDEMPOTENT at a position.
//   - APPLY IS A PURE FUNCTION of (the rows in this transaction, the record,
//     the options). No clock, no config read, no coordination read, no node
//     identity, no randomness, and no collection written from a map without
//     sorting it first.
//
// The purity is not a style rule: it is the whole identity claim. Two nodes
// applying one record must produce the same rows, so anything the applier
// could read from the world instead of from its arguments is a value the two
// nodes can differ on. What an applier legitimately needs from outside
// arrives through [ApplyOptions], which the framework fills once.
//
// There is no Order method and no Reset: a total order removes the first, and
// a checkpoint committed with its own rows removes the second.
type Applier interface {
	// Apply writes this record's rows. The framework commits the rows,
	// the op id, the anchor and the checkpoint in this same transaction.
	Apply(ctx context.Context, tx *sql.Tx, rec Record, opts ApplyOptions) error

	// Committed runs AFTER the transaction commits, for the consequences
	// that are not rows.
	//
	// Separate because the transaction may run more than once: the
	// store's transactions are optimistic and a conflicted one re-runs
	// its body, so a side effect inside Apply would happen twice.
	Committed(ctx context.Context)
}

// ApplyOptions carries every value an applier would otherwise read from the
// world.
//
// Filled ONCE per apply-loop generation by the framework, EXCEPT StoredAt,
// which is per record and comes from the broker — which is what makes it
// byte-identical on every node rather than a clock each node reads for
// itself.
type ApplyOptions struct {
	// Now is the batch's instant. NOT for durations: it is the same
	// value for every record in a batch, so a difference between two
	// records' Now values is a batch boundary rather than elapsed time.
	Now time.Time

	// StoredAt is the broker's own timestamp for this record.
	StoredAt time.Time

	// Epoch is the per-epoch configuration the domain declared it reads.
	// It is here rather than read by the applier because two nodes
	// briefly on different epochs must still produce rows a reader can
	// account for — which is exactly what the Divergent class names.
	Epoch map[string]any
}
