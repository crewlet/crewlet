package iamdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsoncarry"
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
// every later record about the person it removed, with no inverse that repairs
// it — and in THIS domain that is not a stale row, it is a person who has been
// off-boarded still signing in on one node. So [OpRemove]'s payload is pinned
// at 1 and never evolves; a field it needs that it cannot have is a field that
// belongs on a record that is not a gate.
//
// [OpEviction] is pinned at the same version for the same shape of reason one
// layer up: a node that deferred an eviction goes on applying records every
// peer is dropping, and the rows it writes from them have no later record that
// corrects them.
const GateRecordVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpInvite is a claim on an ADDRESS by somebody who has no person yet.
	// Its subject is [KindEmail], create-only at an expectation of zero.
	//
	// ITS OWN OP rather than a shape of [OpClaim], because an invitation
	// is the one claim that names nobody: it holds an address open for a
	// person who does not exist, and what an operator reads in the log is
	// the difference between "this address was invited" and "this address
	// belongs to somebody".
	OpInvite OpKind = "invite"

	// OpClaim binds one address, login or seat to one person. Its subject
	// is [KindEmail], [KindLogin] or [KindSeat], create-only at an
	// expectation of zero.
	//
	// THE CREATE-ONLY EXPECTATION IS THE UNIQUENESS CHECK. There is no
	// unique index anywhere in this estate and there cannot be one, so two
	// administrators claiming one address contend at the broker and exactly
	// one wins. The loser is told which person holds it.
	OpClaim OpKind = "claim"

	// OpRelease gives a claim back: an address changed, a login retired, a
	// person unbound from a seat. Its subject is the claim's own.
	//
	// IT IS NOT A DELETE OF THE CLAIM'S SUBJECT. A released address's
	// subject keeps its arbitration anchor, so a later claim on it is an
	// ordinary conditional write rather than a create at zero — which is
	// what stops a released address being racily re-taken by two people
	// who each read it as free.
	OpRelease OpKind = "release"

	// OpEnrol writes the person row itself. Its subject is [KindPerson].
	//
	// It is the LAST step of an enrolment and not the first: the claims are
	// what can be refused, so they are taken first, and a sequence that
	// stops before this one leaves a claimed address with no person — a
	// legal named state the duplicate-and-orphan duty reports, rather than
	// a person with an address somebody else also holds.
	OpEnrol OpKind = "enrol"

	// OpUpdate is a person's own content, as FULL POST-STATE: their name,
	// their contacts, their grants, their credential set.
	//
	// FULL POST-STATE AND NOT A PATCH, for the chart's reason rather than
	// the tracker's: a person is authored as a form and submitted whole, so
	// the writer always holds the complete new value and a patch would be a
	// diff it computed in order to be reassembled by every node.
	OpUpdate OpKind = "update"

	// OpStatus moves a person between the enrolment and suspension stages
	// [iam.Stage] names. Its subject is [KindPerson].
	//
	// ITS OWN OP AND NOT A FIELD OF [OpUpdate], because suspending somebody
	// is the operation an operator reads the log FOR — and one that must be
	// findable without decoding every content record in the domain.
	// Restoring is the same op with a different status, rather than a
	// second op, so the pair can never be classified apart.
	OpStatus OpKind = "status"

	// OpRevoke bumps a person's REVOCATION EPOCH, which ends every session
	// they hold, everywhere, at once. Its subject is [KindPerson].
	//
	// Three things reach it and they are deliberately one op: signing out
	// everywhere, changing a password, and a session bearer presenting a
	// rotation index past the overlap window — which is REUSE, and the only
	// safe reading of reuse is that somebody else has the cookie.
	//
	// IT CARRIES A [MutationRecord.Reason] because the three are
	// indistinguishable afterwards and an operator investigating a
	// compromise needs to know which one fired.
	OpRevoke OpKind = "revoke"

	// OpRemove is a person the company no longer has, and it INSTALLS A
	// GATE by its op rather than by its kind: its subject is the ordinary
	// person subject.
	//
	// The gate exists because a removal is the one operation here with no
	// inverse a later record supplies. Every other record is a full
	// post-state under a monotone guard, so a node that deferred one is
	// repaired by the next record about that person; nothing ever names a
	// removed person again, so a node that deferred a removal would go on
	// admitting somebody every peer has off-boarded.
	//
	// WHAT IT ACTUALLY DESTROYS is the person's data encryption key, which
	// is what makes their name and address unrecoverable from an artefact
	// taken before it. The row and the id outlive it, because the audit
	// trail names them.
	OpRemove OpKind = "remove"

	// OpOpen begins one session. Its subject is [KindSession], create-only
	// at an expectation of zero, so a lineage can be opened exactly once.
	OpOpen OpKind = "open"

	// OpClose ends one session. Its subject is [KindSession].
	//
	// ROTATIONS ARE NOT RECORDS and this is the reason the pair is only
	// two: a rotation id is an HMAC over the lineage and the session's age
	// in rotate_after units, so the busiest thing a signed-in person does
	// writes nothing on this log at all. What lands here is a session
	// beginning and a session ending, which is a handful of records per
	// person per day.
	OpClose OpKind = "close"

	// OpBootstrap moves the company's ONE bootstrap through its own life:
	// minted, redeemed, or expired. Its subject is [KindBootstrap].
	//
	// ONE OP FOR THE WHOLE LIFE, unlike a session's pair, because there is
	// exactly one bootstrap object and its records sit consecutively on one
	// subject: an operator reads them in order whatever they are called,
	// and a second op would buy a word at the cost of a second thing the
	// classification table has to name.
	OpBootstrap OpKind = "bootstrap"

	// OpSweep deletes one bucket's expired rows below a POSITION the
	// publisher resolved once. Its subject is [KindSweep].
	//
	// A RECORD RATHER THAN A LOCAL DELETE, because these rows are
	// identity-claimed: a node sweeping on its own clock would hold
	// different bytes from its peers, and "N byte-identical copies" would
	// quietly become a claim about how synchronised their clocks were.
	OpSweep OpKind = "sweep"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpEviction gates a node's records on this log, or readmits it. Its
	// subject is [KindEviction].
	//
	// PINNED AT [GateRecordVersion], like [OpRemove].
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's own record, on [KindGeneration].
	OpGeneration OpKind = "generation"
)

// OpKinds are the fourteen, in the order they are documented.
var OpKinds = []OpKind{
	OpInvite, OpClaim, OpRelease, OpEnrol, OpUpdate, OpStatus, OpRevoke,
	OpRemove, OpOpen, OpClose, OpBootstrap, OpSweep, OpBarrier,
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
//
// # NOTHING HERE IS PERSONAL DATA, and that is a property of the format
//
// Every field in this struct is readable by every build for ever, which means
// it is also readable by every OPERATOR SURFACE that renders a record this
// build could not decode: a deferral row, a stalled-log report, a broker
// listing. So none of them carries a name, an address or a login — the subject
// is a blind or an opaque id, and the scope is a bucket number. A person's own
// values live in the payload, sealed under their own key, which the envelope
// pass never opens.
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

	// Subject is the claim this record arbitrates on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock. It rides
	// the record so an operator reading an authentication trail can see
	// when a revocation was decided, and it is NEVER ORDERED ON and never
	// written to a row: every instant this domain stores is the broker's,
	// which is what makes one node's copy byte-identical to another's.
	// Empty on a barrier, because nothing renders one.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation. It is what makes a row's version a pure
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

	// Scope is the complete set of identity buckets this record's apply may
	// write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate.
//
// ANSWERED FROM THE ENVELOPE ALONE, because it must be answerable by a node
// that cannot decode the payload: it is what turns an unknown version into a
// STOP rather than a deferral.
//
// ON THE OP AND NOT ON A KIND, for the chart's reason: a removal rides the
// ORDINARY PERSON SUBJECT, so a reader that keyed on the kind would defer it —
// and a deferred removal here is a person the company off-boarded still
// signing in on one node. An eviction has a kind of its own and could have
// been read either way; it is read here so the question is asked once, in one
// place, for both.
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
	//
	// IT IS WHERE EVERY SEALED VALUE LIVES. A person's name and address are
	// sealed under that person's own key BEFORE publication, with their id
	// as the additional authenticated data, so the bytes on the broker, in
	// every node's deferred table and in every snapshot are ciphertext that
	// only a node holding the key can open — and destroying the key is what
	// a removal actually does.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	// Person is the id every row this record writes belongs to, IN THE
	// CLEAR, and it is here rather than in the envelope on purpose.
	//
	// The applier needs it to attach rows the subject does not name: a
	// claim arbitrates on a blind, a login or a seat id, and the person is
	// what its row is keyed on. The ENVELOPE does not carry it because
	// every envelope field is rendered by an operator surface for a record
	// nobody could decode, and a person's id in a stalled-log report is a
	// durable, replicated statement that this person exists.
	//
	// Empty on a barrier, a sweep, an eviction, a generation and an
	// invitation — the five records that are about nobody.
	Person string `json:"person,omitempty"`

	// Actor and ActorKind are who did this.
	//
	// THE KIND IS A COLUMN AND NEVER A PREFIX ON THE NAME, which is
	// internal/iam's own rule: an operator prefixed into a name showed one
	// person as two people three rows apart in the audit feed.
	Actor     string   `json:"actor,omitempty"`
	ActorKind iam.Kind `json:"actor_kind,omitempty"`

	// Reason is why, in at most [MaxReason] bytes, for the operations
	// whose motive is not recoverable from their effect.
	//
	// A REVOCATION IS THE CASE IT EXISTS FOR: signing out everywhere, a
	// password change and detected token reuse produce an identical epoch
	// bump, and which one fired is the first question anybody investigating
	// a compromise asks. It is prose an operator wrote or a constant the
	// engine chose, never a stack trace and never a value from a request.
	Reason string `json:"reason,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT on the record, deliberately.
//
// THE BROKER'S INSTANT. The effective instant every duration is measured
// against derives from the broker's own store timestamp, which the replication
// loop stamps onto the record before calling the applier. A writer must not be
// able to author it, and a field a writer could set is a field a writer could
// lie about — so it reaches the applier as an argument rather than as a key in
// the format.
//
// A NOTIFY BLOCK. The chart carries one because a chart change wakes the seats
// it moved. An identity change wakes nobody: a person being suspended, a
// session ending or an address being claimed is not work for an agent, and the
// contact updates that DO follow a status change are derived by a duty from
// the rows rather than routed from the record. A field with no producer is a
// value every reader is entitled to misread, so there is none.
//
// AN IP ADDRESS, A USER AGENT OR A DEVICE FINGERPRINT. A session record says
// that a session was opened and by whom; where from is a request-scoped
// observation, and putting one on a replicated, permanently retained log would
// make every node's database a location history nobody asked for.

// MaxReason bounds the reason a record carries.
//
// TWO HUNDRED AND FIFTY-SIX, which is a line of prose: this is rendered into
// an authentication trail beside the op that caused it, where the op is
// already the headline, so the field's job is to say WHICH of three
// indistinguishable causes fired rather than to narrate.
const MaxReason = 256

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
	return fmt.Sprintf("iamdomain: the record on %s is version %d and this "+
		"build reads %d — it is RETAINED at its position rather than skipped, "+
		"and reprocessed by a build that knows the shape", e.Subject, e.Got,
		e.Want)
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
		return RecordEnvelope{}, fmt.Errorf("iamdomain: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("iamdomain: a record carries "+
			"version %d — every record states its version, and one that does "+
			"not cannot be told apart from a newer build's", env.V)
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
	extra, err := jsoncarry.Decode(payload, &rec, recordFields)
	if err != nil {
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("iamdomain: "+
			"decode the record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote.
//
// LOSSLESS IN BOTH DIRECTIONS, which is what makes a rolling upgrade safe: a
// node that read a record it only half understood and republished it — the
// reanchor path does exactly that — must not strip the half it did not.
func Encode(rec MutationRecord) ([]byte, error) {
	data, err := jsoncarry.Encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: encode the record on %s: %w",
			rec.Subject, err)
	}
	return data, nil
}

// recordFields is every top-level name this build writes.
//
// DERIVED from the struct rather than typed again, on [jsoncarry.Names]'
// terms: the omitempty names have to be listed because a zero value does not
// marshal them, and a name missing here is decoded into the struct AND carried
// as unknown — so the next encode writes the stale carried copy back over what
// the caller set.
var recordFields = jsoncarry.Names(MutationRecord{}, "op_id", "created_at",
	"gen", "writer", "expect", "mutation", "person", "actor", "actor_kind",
	"reason")
