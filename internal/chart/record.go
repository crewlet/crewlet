package chart

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// RecordVersion is the record shape THIS BUILD can decode.
//
// A record above it is RETAINED rather than skipped — see the deferral contract
// in [statelog] — which is what makes a rolling upgrade a period of reduced
// coverage rather than an outage.
const RecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER.
//
// A removal whose version this build could not read would be deferred, and a
// deferred gate does not postpone one record's effect on one node: it licenses
// every later record on the objects the removal destroyed, with no inverse that
// repairs it. So [OpRemove]'s payload is pinned at 1 and never evolves — a
// field it needs that it cannot have is a field that belongs on the structural
// record that accompanies it.
//
// [OpEviction] is pinned at the same version for the same shape of reason one
// layer up: a node that deferred an eviction goes on applying records every
// peer is dropping, and the rows it writes from them have no later record that
// corrects them.
const GateRecordVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpPlace is the structural record: where a unit sits, which unit a
	// seat sits in, and which seat leads a unit. Its subject is
	// [KindTree].
	//
	// ONE OP FOR ALL THREE, because all three are the same fact — an edge
	// in the tree — and a chart is only ever wrong as a whole. Splitting
	// them would let a reparent and a lead change on one unit commit
	// independently, which is two writers each holding half of one
	// decision.
	OpPlace OpKind = "place"

	// OpImport is a whole chart applied from one company revision: the
	// batch, and the record whose enumeration can exceed [MaxScopeTerms]
	// and collapse to the root term.
	//
	// ITS OWN OP rather than a large [OpPlace], because what an operator
	// reads in the log is the difference between "somebody moved a seat"
	// and "a config revision rewrote the chart", and reconstructing that
	// from the size of a payload is not reading, it is guessing.
	OpImport OpKind = "import"

	// OpRemove is what the chart no longer names, and it INSTALLS A GATE by
	// its op rather than by its kind: its subject is the ordinary structure
	// subject.
	//
	// The gate exists because a removal is the one operation here with no
	// inverse a later record supplies. Every other record is a full
	// post-state under a monotone guard, so a node that deferred one is
	// repaired by the next record on that object; nothing ever names a
	// removed object again, so a node that deferred a removal keeps the
	// object for ever while every peer has dropped it.
	OpRemove OpKind = "remove"

	// OpUpsert is an object's own content, as FULL POST-STATE. Its subject
	// is [KindUnit] or [KindSeat].
	//
	// FULL POST-STATE AND NOT A PATCH, which is the opposite of the
	// tracker's and the knowledge base's choice for their largest objects,
	// and the reason is where the write comes from: a chart is authored as
	// a DOCUMENT and reconciled whole, so the writer always holds the
	// complete new value and a patch would be a diff it computed in order
	// to be reassembled by every node. The objects are also small — a seat
	// is prose bounded at [MaxProse] per field — where a page is half a
	// mebibyte and a patch genuinely saves the wire.
	OpUpsert OpKind = "upsert"

	// OpRekey moves one KEY onto one object, keeping the old one resolving.
	// Its subject is [KindRekey] — the key being claimed.
	OpRekey OpKind = "rekey"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpEviction gates a node's records on this log, or readmits it. Its
	// subject is [KindEviction].
	//
	// PINNED AT [GateRecordVersion], like [OpRemove] and for the same
	// reason one layer up: a node that deferred an eviction goes on
	// applying records every peer is dropping, and there is no later
	// record that repairs the rows it wrote from them.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's own record, on [KindGeneration].
	OpGeneration OpKind = "generation"
)

// OpKinds are the eight, in the order they are documented.
var OpKinds = []OpKind{
	OpPlace, OpImport, OpRemove, OpUpsert, OpRekey, OpBarrier,
	OpEviction, OpGeneration,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are RESERVED at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. It is
// the same split the event envelope makes between a known header and an opaque
// body, for the same reason — a rolling upgrade puts a record this build cannot
// read on the wire, and dropping it would make every upgrade an outage.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown, never
	// for this struct.
	V int `json:"v"`

	// OpID is a uuid7: the idempotency key, the Nats-Msg-Id, and the ops
	// table's key. EMPTY on a barrier, deliberately — an op id is what
	// invites a message id, and a duplicate ack is served out of the
	// dedupe window with no quorum round trip at all, which is the one
	// thing a read barrier must never be.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates, and the unit the broker
	// arbitrates it on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. It rides
	// the record so an operator reading the log can see when a
	// reorganisation was decided, and it is NEVER ORDERED ON and never
	// written to a row: every instant this domain stores is the broker's,
	// which is what makes one node's copy of the chart byte-identical to
	// another's. Empty on a barrier, because nothing renders one.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes an object's version a pure
	// function of the record — (Gen << 40) | the broker's own sequence — so
	// the applier needs no table lookup, no config read and no clock.
	// Present on a barrier too, because a barrier from a previous
	// generation must be refusable.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate — which
	// runs before any kind rule, on a node that may not be able to decode
	// the payload at all. Empty on a barrier, which writes no rows for a
	// gate to drop.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of objects this record's apply may write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate.
//
// ANSWERED FROM THE ENVELOPE ALONE, because it must be answerable by a node
// that cannot decode the payload: it is what turns an unknown version into a
// STOP rather than a deferral.
//
// ON THE OP AND NOT ON A KIND, and a removal is why: it rides the ORDINARY
// STRUCTURE SUBJECT, so a reader that keyed on the kind would defer it, and a
// deferred removal is a node that goes on serving a unit every other node has
// dropped. An eviction has a kind of its own and could have been read either
// way; it is read here so that the question is asked once, in one place, for
// both — a gate rule written twice is a record that stops the applier on the
// write side and is filed for later on the read side.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Op == OpRemove || e.Op == OpEviction
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the version this mutation was decided against. PROVENANCE
	// ONLY — the broker is what enforced it, and nothing reads this to
	// decide anything.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as OPAQUE
	// BYTES when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`
	TurnID     string     `json:"turn_id,omitempty"`

	// Revision is the company configuration revision this change came
	// from, when it came from one.
	//
	// ON THE RECORD rather than looked up, because it is the answer to the
	// question an operator asks about a chart they did not expect: which
	// edit did this. A change made through a tool carries none, and that
	// absence is itself the answer — somebody did it by hand.
	//
	// It is also what `chart_import_ledger` is keyed on, so a re-import of
	// a revision the chart already holds is a no-op every node reaches the
	// same way rather than a second pass that rewrites every row.
	Revision string `json:"revision,omitempty"`

	// Notify is the routing snapshot, and NIL is what "wakes nobody" means.
	//
	// A NIL POINTER RATHER THAN A quiet FLAG, so the wake filter is "has a
	// Notify" — a question about the record's own shape — instead of a
	// boolean a writer can forget to set. A quiet commit is a full record,
	// arbitrated exactly like a loud one, and writes its history row like
	// every other: quiet means it wakes nobody, and nothing else.
	//
	// AN IMPORT IS ORDINARILY QUIET, which is this domain's own version of
	// the rule a page's comment follows: a config revision that touches
	// forty seats would otherwise wake forty seats to tell each of them
	// their goal was reworded.
	Notify *Notify `json:"notify,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT in the envelope, deliberately: the BROKER'S instant.
//
// The effective instant every duration is measured against derives from the
// broker's own store timestamp, which the replication loop stamps onto the
// record before calling the applier. A writer must not be able to author it,
// and a field a writer could set is a field a writer could lie about — so it
// reaches the applier as an argument rather than as a key in the format.
// Stated here so nobody "completes" the envelope by adding it.

// Notify is the routing snapshot a wake is derived from, copied at write time
// so the node that wins a feed message routes without reading anything.
//
// EVERYTHING A NOTIFICATION IS BUILT FROM IS HERE, and that is the contract
// rather than a convenience: the feed relays the RECORD, and the node that wins
// that message may be one whose applier has not reached the change — so a
// parser that read a chart row instead would route from a stale tree, or block
// the feed until it had caught up.
type Notify struct {
	// Kind is what happened, in the vocabulary a card renders.
	Kind ChangeKind `json:"kind"`

	// Object is what the change is about.
	//
	// ON THE NOTIFICATION rather than taken from the subject, because the
	// records that wake somebody most are the ones whose subject is NOT the
	// object: a placement and a removal both arbitrate on the structure,
	// and the seat that moved is inside a payload whose shape a parser
	// would otherwise have to decode per op.
	Object ObjectRef `json:"object"`

	// Recipients are the handles woken, computed once at write time so the
	// feed never has to resolve a tree and can never forget to.
	//
	// WHO THEY ARE IS THE WRITER'S JUDGEMENT AND NOT A RULE HERE, for the
	// reason the tracker's routing precedence is order in one function: a
	// move's recipients are the seat that moved, the lead it left and the
	// lead it joined, and stating that as a list on the record is what
	// makes it the same list on every node.
	Recipients []string `json:"recipients,omitempty"`

	// Summary is at most [MaxSummary] bytes of what a card should show.
	Summary string `json:"summary,omitempty"`
}

// MaxSummary bounds the summary a change record carries.
//
// SIX HUNDRED, the same as a page change's excerpt, and for the same reason: it
// is rendered into a wake prompt beside everything else a turn is handed, so it
// is a token budget rather than a storage one.
const MaxSummary = 600

// ErrFutureVersion reports a record a newer build wrote.
//
// It carries the SUBJECT because the caller is the applier's deferral arm,
// which has to file the record under something — and a version error with no
// subject is a record that can only be dropped.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("chart: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
//
// Every branch that makes an un-decodable record survivable turns on something
// here: the subject it is filed under, the scope a writer probes for, the op a
// gate reads, and the version that decides whether there is a second pass at
// all.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("chart: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("chart: a record carries version "+
			"%d — every record states its version, and one that does not "+
			"cannot be told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the SECOND pass, and it is the one that may refuse on version.
//
// It returns the envelope alongside the error whenever the envelope itself
// decoded, because that is precisely the case the applier retains: the caller
// needs the subject, the scope and the op of a record it cannot read.
func Decode(payload []byte) (MutationRecord, error) {
	env, err := DecodeEnvelope(payload)
	if err != nil {
		return MutationRecord{}, err
	}
	if env.V > RecordVersion {
		return MutationRecord{RecordEnvelope: env}, &ErrFutureVersion{
			Got: env.V, Want: RecordVersion, Subject: env.Subject,
		}
	}
	var rec MutationRecord
	extra, err := decodeInto(payload, &rec, recordFields)
	if err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("chart: decode "+
			"the record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote.
//
// LOSSLESS IN BOTH DIRECTIONS, which is what makes a rolling upgrade safe: a
// node that read a record it only half understood and republished it — the
// reanchor path does exactly that — must not strip the half it did not.
//
// It goes through the same [encode] every document shape here uses, rather than
// a second merge of its own: a carried field LOSES to a known one, and two
// implementations of that rule are one place where a stale carried copy undoes
// the write that set it.
func Encode(rec MutationRecord) ([]byte, error) {
	data, err := encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("chart: encode the record on %s: %w",
			rec.Subject, err)
	}
	return data, nil
}

// recordFields is every top-level name this build writes.
//
// DERIVED from the struct rather than typed again, on [fieldSet]'s terms: the
// omitempty names have to be listed because a zero value does not marshal them,
// and a name missing here is decoded into the struct AND carried as unknown —
// so the next encode writes the stale carried copy back over what the caller
// set.
var recordFields = fieldSet(MutationRecord{}, "op_id", "created_at", "gen",
	"writer", "expect", "mutation", "actor", "actor_kind", "operator_id",
	"turn_id", "revision", "notify")
