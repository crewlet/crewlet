package pages

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// RecordVersion is the record shape THIS BUILD can decode.
//
// A record above it is RETAINED rather than skipped — see the deferral
// contract in [statelog] — which is what makes a rolling upgrade a period of
// reduced coverage rather than an outage.
const RecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER.
//
// An eviction whose version this build could not read would be deferred, and a
// deferred gate leaves this node's own gate table empty while it goes on
// applying every record the evicted node appends — with no inverse that
// repairs it. So the two gate kinds are pinned at 1 and never evolve: a field
// they need that they cannot have is a field that belongs somewhere else.
const GateRecordVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpCreate makes a page. Its subject is the TITLE, not the page: the
	// address is what two writers contend for.
	OpCreate OpKind = "create"

	// OpPatch changes a page head — its body, its labels, its watchers, a
	// comment on it — or a container's settings.
	OpPatch OpKind = "patch"

	// OpRename moves a page to a new address. Its subject is the NEW
	// title, create-only, and its scope names the old one too.
	OpRename OpKind = "rename"

	// OpTombstone trashes a page, and OpRestore takes it back. Both are
	// reversible and neither removes a row.
	OpTombstone OpKind = "tombstone"
	OpRestore   OpKind = "restore"

	// OpPurge removes a page permanently, and INSTALLS A GATE by its op
	// rather than by its kind: its subject is an ordinary page.
	OpPurge OpKind = "purge"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"
)

// OpKinds are the nine, in the order they are documented.
var OpKinds = []OpKind{
	OpCreate, OpPatch, OpRename, OpTombstone, OpRestore, OpPurge,
	OpEviction, OpGeneration, OpBarrier,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are RESERVED at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. It is
// the same split the event envelope makes between a known header and an opaque
// body, for the same reason — a rolling upgrade puts a record this build
// cannot read on the wire, and dropping it would make every upgrade an outage.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown,
	// never for this struct.
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

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. Reported,
	// never ORDERED ON, and differenced against exactly one thing — the
	// broker's stored instant, to publish a skew. Empty on a barrier,
	// because nothing renders one.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes an object's version a pure
	// function of the record — (Gen << 40) | the broker's own sequence —
	// so the applier needs no table lookup, no config read and no clock.
	// Present on a barrier too, because a barrier from a previous
	// generation must be refusable.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate —
	// which runs before any kind rule, on a node that may not be able to
	// decode the payload at all. Empty on a barrier, which writes no rows
	// for a gate to drop.
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
// TWO CONDITIONS, NOT ONE. An eviction is a gate by its KIND; a purge is a
// gate by its OP, on an ordinary page subject. Reading only the kind would let
// a purge this build cannot decode be deferred — and a deferred purge is a
// node that goes on serving a page every other node has removed, with no
// inverse that repairs it.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
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
	Chain      []string   `json:"chain,omitempty"`

	// Notify is the routing snapshot, and NIL is what "wakes nobody"
	// means.
	//
	// A NIL POINTER RATHER THAN A quiet FLAG, so the wake filter is "has a
	// Notify" — a question about the record's own shape — instead of a
	// boolean a writer can forget to set. A quiet commit is a full record,
	// arbitrated exactly like a loud one, and writes its history row like
	// every other: quiet means it wakes nobody, and nothing else.
	//
	// A COMMENT DOES NOT SUBSCRIBE ITS COMMENTER here, which is the
	// opposite of the tracker's participants rule and deliberate: a page a
	// hundred people have remarked on would otherwise wake a hundred seats
	// when somebody fixes a heading.
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
type Notify struct {
	// Kind is what happened, in the vocabulary a card renders.
	Kind ChangeKind `json:"kind"`

	// Recipients is the watcher set MINUS the muted, computed once at
	// write time so the feed never has to subtract and can never forget
	// to.
	Recipients []string `json:"recipients,omitempty"`

	// Mentions are the handles a body named, which are woken whether or
	// not they watch.
	Mentions []string `json:"mentions,omitempty"`

	// Excerpt is at most [MaxExcerpt] bytes of what a card should show.
	Excerpt string `json:"excerpt,omitempty"`

	// Container and Title are the page's address at the moment of the
	// change, so a wake reads without a lookup.
	Container string `json:"container,omitempty"`
	Title     string `json:"title,omitempty"`
}

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
	return fmt.Sprintf("pages: the record on %s is version %d and this build "+
		"reads %d — it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
//
// Every branch that makes an un-decodable record survivable turns on something
// here: the subject it is filed under, the scope a writer probes for, the kind
// a gate reads, and the version that decides whether there is a second pass at
// all.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("pages: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("pages: a record carries version "+
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
// needs the subject, the scope and the kind of a record it cannot read.
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
	extra, err := decodeInto(payload, &rec, recordFields, env.V)
	if err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("pages: decode "+
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
// It goes through the same [encode] every document shape here uses, rather
// than a second merge of its own: a carried field LOSES to a known one, and
// two implementations of that rule are one place where a stale carried copy
// undoes the write that set it.
func Encode(rec MutationRecord) ([]byte, error) {
	data, err := encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("pages: encode the record on %s: %w",
			rec.Subject, err)
	}
	return data, nil
}

// recordFields is every top-level name this build writes.
//
// DERIVED from the struct rather than typed again, on [fieldSet]'s terms: the
// omitempty names have to be listed because a zero value does not marshal
// them, and a name missing here is decoded into the struct AND carried as
// unknown — so the next encode writes the stale carried copy back over what
// the caller set.
var recordFields = fieldSet(MutationRecord{}, "op_id", "created_at", "gen",
	"writer", "expect", "mutation", "actor", "actor_kind", "operator_id",
	"turn_id", "chain", "notify")
