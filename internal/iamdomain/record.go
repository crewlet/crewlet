package iamdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsoncarry"
	"github.com/crewlet/crewlet/internal/statelog"
)

// RecordVersion is the record shape THIS BUILD can decode; a record above it
// is RETAINED rather than skipped (the deferral contract in [statelog]).
//
// IT IS THE CEILING THIS BUILD READS, NOT WHAT IT WRITES: [Encode] stamps every
// record with the lowest version that reads it, over [versionedFields], because
// a peer defers a record it cannot read. It moves only with that table, exactly
// to its highest version. Version 1 is every shape this domain has.
const RecordVersion = 1

// BaseRecordVersion is version 1, the base format every build reads and the
// stamp of a record carrying no versioned field. A gate, the read index's
// barrier and a reanchor's generation stay at it
// ([MutationRecord.readByEveryBuild]), save a gate whose APPLY changed, which
// rides a version an older build halts at.
const BaseRecordVersion = 1

// GateRecordVersion is the SHAPE version every gate's payload (a removal's, an
// invalidation's, an eviction's) and a reanchor's generation carries, FOR EVER.
//
// An undecodable gate STOPS the applier rather than being retained, since a
// deferred gate has no later record to repair it; pinned, a gate's shape only
// grows by addition, so no shape change takes an older node out. A generation
// is pinned beside them because a retained one is a transition never made. A
// change to what a gate's APPLY does is a [versionedFields] row carrying
// [statelog.VersionedField.Gate], at which older builds halt; never a new op,
// which an older build would not know as a gate and so would defer. The rule
// is the framework's; internal/worklet's GateRecordVersion states it at length.
const GateRecordVersion = 1

// versionedFields is every field an identity record has gained since the base
// format, with the version a reader must be at to apply a record carrying it:
// the one table [Encode] stamps from. EMPTY: this build writes the base format.
//
// ADDING A FIELD TO ANY RECORD OR PAYLOAD IS ADDING A ROW HERE: an older build
// decodes around a field it has no home for, so its rows diverge on tables the
// identity claim compares, or a missing CONDITION reads as none and permits
// more. The row stamps a version that build retains; one added after a release
// takes the next version above [RecordVersion], which moves with it, and the
// statelogtest candidate gains a record carrying it. A new predicate with no
// field of its own (a sweep that collects more) is a marker on the record's
// root, and the marker gets the row.
var versionedFields = statelog.RecordFields{}

// VersionedFields is the table, for the conformance suite and for an operator
// surface that names why a record was held back.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

const (
	// OpInvite holds an address and a human seat for somebody with no
	// person yet, on [KindDirectory]: the decide refuses either while a
	// person or another open invitation holds it, and the invitation holds
	// both until it is redeemed, cancelled or ages out. ITS OWN OP rather
	// than a shape of [OpEnrol] because it names nobody, and the log must
	// tell "this address was invited" from "this address belongs to
	// somebody".
	OpInvite OpKind = "invite"

	// OpCancel withdraws an unredeemed invitation, on [KindDirectory]; its
	// apply DELETES the row and its sealed copy, freeing the address and
	// the seat at once. ITS OWN OP rather than a deadline moved to now, so
	// the trail says who withdrew it. A REDEEMED invitation is never
	// cancelled: what undoes it is removing the person it created.
	OpCancel OpKind = "cancel"

	// OpEnrol creates a person WHOLE (row, first credential, login, address,
	// seat) and, for a redemption, spends the invitation, in ONE record on
	// [KindDirectory]. [Writer.Create] publishes it with the FIRST PASSWORD
	// LINK as a `reset` credential stamped with the record's own snapshot
	// counters; [Writer.Redeem] with the password the invitee chose.
	//
	// ONE RECORD because the directory's decide judges every half in one
	// snapshot (address, login and seat unheld, invitation unspent); split,
	// a landed half is a reservation every reader steps around and nothing
	// finishes.
	OpEnrol OpKind = "enrol"

	// OpIdentity sets an enrolled person's LOGIN and SEAT binding, as
	// post-state, on [KindDirectory]. BOTH ARE ALWAYS STATED (the unchanged
	// one as the snapshot held it), so a rename, bind, seat move or service
	// account's unbind frees the old value in the record that takes the
	// new: there is no release step. A person is never unbound: their seat
	// is moved, or freed by their removal ([ErrSeatRequired]).
	OpIdentity OpKind = "identity"

	// OpUpdate is a person's own content (name, contacts, grants, credential
	// set) as FULL POST-STATE, not a patch: a person is submitted whole as a
	// form, so a patch would be a diff every data node reassembles.
	OpUpdate OpKind = "update"

	// OpStatus moves a person between the stages [iam.Stage] names, on
	// [KindPerson]. ITS OWN OP, not a field of [OpUpdate], because a
	// suspension is what an operator reads the log for and must be findable
	// without decoding content records; restoring is the same op, so the
	// pair can never be classified apart.
	OpStatus OpKind = "status"

	// OpRevoke bumps a person's REVOCATION EPOCH, ending every session they
	// hold, everywhere, on [KindPerson]. Signing out everywhere, an
	// administrator ending their sessions and a second factor reset are
	// deliberately one op; it carries a [MutationRecord.Reason] because
	// only that tells the three apart when investigating a compromise.
	OpRevoke OpKind = "revoke"

	// OpPassword replaces a person's password AND moves their revocation
	// epoch in ONE record on [KindPerson]: the person's document as full
	// post-state (new verifier, every outstanding reset link revoked)
	// beside the new epoch. Either half alone is a state somebody can be
	// left in: a changed password with live sessions did not do its job.
	// ITS OWN OP, not [OpUpdate] or [OpRevoke], so "when did this password
	// change, and by them or a reset link" is findable, the reason naming
	// which.
	OpPassword OpKind = "password"

	// OpRemove is a person the company no longer has. It rides the
	// DIRECTORY subject (it frees a login, an address and a seat), so it
	// INSTALLS A GATE by its op, not its kind: it is the one record no later
	// record repairs, since nothing names a removed person again, and a node
	// that deferred it would go on admitting them.
	//
	// It ERASES every sealed value of theirs: their rows go, and the
	// invitation and trail rows that outlive them lose their sealed columns.
	// The tombstone and the id remain, because the audit trail names them.
	OpRemove OpKind = "remove"

	// OpOpen begins one session. Its subject is [KindSession], create-only
	// at an expectation of zero, so a lineage can be opened exactly once.
	OpOpen OpKind = "open"

	// OpClose ends one session, on [KindSession]. A RE-ISSUE IS NOT A
	// RECORD: the idle deadline it moves lives in the bearer's own signed
	// payload, so this log sees a handful of records per person per day.
	OpClose OpKind = "close"

	// OpInvalidate ends EVERY session in the company by moving the
	// fleet-wide generation a bearer carries, on [KindInvalidation]. It
	// INSTALLS A GATE: a node that deferred it would honour every cookie
	// just invalidated, with no later record to repair it.
	//
	// NOT [OpRevoke] AT SCALE: a restore rolls a per-person epoch back with
	// every other row, while this is the one number that can be pushed
	// forward without knowing who was affected, which the restore runbook's
	// last step needs. The new generation is STATED, never `+ 1` in the
	// applier, which would fold over an arrival order nodes do not share.
	OpInvalidate OpKind = "invalidate"

	// OpSweep deletes one bucket's expired rows below a POSITION the
	// publisher resolved once, on [KindSweep]. A RECORD, not a local delete,
	// because the rows are identity-claimed: each node sweeping on its own
	// clock would hold different bytes.
	OpSweep OpKind = "sweep"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record that removes the
	// arbitration anchors below a floor (ADR-0080). Its subject is
	// [KindTrimmed].
	OpTrimmed OpKind = "trimmed"

	// OpEviction gates a node's records on this log, or readmits it. Its
	// subject is [KindEviction].
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's own record, on [KindGeneration].
	OpGeneration OpKind = "generation"
)

// OpKinds are the seventeen, in the order they are documented.
var OpKinds = []OpKind{
	OpInvite, OpCancel, OpEnrol, OpIdentity, OpUpdate, OpStatus, OpRevoke,
	OpPassword, OpRemove, OpOpen, OpClose, OpInvalidate, OpSweep, OpBarrier,
	OpTrimmed, OpEviction, OpGeneration,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted, so a record
// this build cannot read is retained rather than dropped. Its EIGHT top-level
// keys are RESERVED for the format's life: a later version may add fields
// beside them, never repurpose one.
//
// NOTHING HERE IS PERSONAL DATA, by design of the format: every operator
// surface that renders an undecodable record (a deferral row, a stalled-log
// report, a broker listing) reads these fields. So the subject is an opaque id
// or names nobody and the scope is a bucket number; a person's values live in
// the payload, sealed under the fleet keyring, which the envelope pass never
// opens.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown, never
	// for this struct.
	V int `json:"v"`

	// OpID is the operation id ([statelog.NewOpID]'s grammar): the
	// idempotency key, the Nats-Msg-Id, the ops table's key, and the
	// instant it was minted at, which the publisher reads to decide whether
	// this node's ledger can vouch for a retry. A gesture's step carries the
	// gesture's id and step ([statelog.StepOpID]). EMPTY on a barrier (an op
	// id invites a message id, whose duplicate ack skips the quorum round
	// trip a barrier exists for) and on a trimmed record, whose identical
	// copies are each meant to land.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record arbitrates on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is THE AUTHORED INSTANT, the writer's own clock, for the
	// authentication trail an operator reads. NEVER ORDERED ON and never
	// written to a row: every instant this domain stores is the broker's,
	// which keeps the nodes' copies byte-identical. Empty on a barrier.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation: it makes a row's version a pure function of
	// the record, (Gen << 40) | the broker's sequence, with no lookup,
	// config read or clock. Present on a barrier too, so one from a
	// previous generation can be refused.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate, which
	// runs before any kind rule on a node that may not decode the payload.
	// Empty on a barrier, which writes no rows; SET on a trimmed record,
	// which removes anchors, so an evicted node's must be droppable.
	//
	// IT AND Gen ARE THE FRAMEWORK'S STAMP ([statelog.Stamp]), taken from
	// the stamp a decide is handed (a reanchor's from the facts the
	// transition hands [GenerationRecord]) and nowhere else: the publisher
	// refuses a record not carrying the one it was decided under.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of identity buckets this record's apply may
	// write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate.
//
// ANSWERED FROM THE ENVELOPE ALONE, so a node that cannot decode the payload
// turns an unknown version into a STOP rather than a deferral. ON THE OP, not
// the kind: a removal rides the DIRECTORY subject beside every enrolment, so a
// kind-keyed reader would defer it, leaving somebody off-boarded signing in on
// one node. Eviction and invalidation are read here too, so the question is
// asked in one place.
func (e RecordEnvelope) InstallsGate() bool {
	switch e.Op {
	case OpRemove, OpInvalidate, OpEviction:
		return true
	}
	return false
}

// MutationRecord is one committed mutation: the envelope plus everything a
// build at this version may read.
type MutationRecord struct {
	RecordEnvelope

	// Expect is the version this mutation was decided against. PROVENANCE
	// ONLY: the broker enforced it, and nothing reads this to decide.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as OPAQUE
	// BYTES when the version is above this build's. EVERY SEALED VALUE LIVES
	// HERE, sealed under the fleet keyring BEFORE publication with the
	// person's id and the field as additional authenticated data, so the
	// broker, every deferred table and every snapshot hold ciphertext only.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	// Person is the id every row this record writes belongs to, IN THE
	// CLEAR: the applier keys rows on it where the subject does not name
	// them (a directory subject names nobody, a session's names a lineage).
	// NOT in the envelope, which operator surfaces render for undecodable
	// records: an id in a stalled-log report is a durable, replicated
	// statement that this person exists.
	//
	// Empty on a barrier, a trimmed record, a sweep, an eviction, a
	// generation, an invalidation and an invitation's issue: the records
	// about nobody.
	Person string `json:"person,omitempty"`

	// Actor and ActorKind are who did this. THE KIND IS A COLUMN, NEVER A
	// PREFIX ON THE NAME (internal/iam's rule), or one person reads as two
	// in the audit feed.
	Actor     string   `json:"actor,omitempty"`
	ActorKind iam.Kind `json:"actor_kind,omitempty"`

	// OperatorID is the credential Actor acted THROUGH (a machine token's
	// `pat:<id>`, a browser session's `session:<lineage>`, a Tier A token's
	// login), which tells a token's gesture from its owner's. A sign-in
	// surface gesture is written FOR the person ([Writer.For]): the session
	// it opened or closed, or their login before any session. Empty for the
	// node's own writer: every duty, and what the engine decides itself,
	// such as a password re-hashed at this build's cost.
	OperatorID string `json:"operator_id,omitempty"`

	// Reason is why, in at most [MaxReason] bytes, for an op whose effect
	// does not show its motive; a revocation is the case it exists for
	// ([OpRevoke]). Prose an operator wrote or a constant the engine chose,
	// never a stack trace or a value from a request.
	Reason string `json:"reason,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// What is NOT on the record, deliberately.
//
// THE BROKER'S INSTANT: durations measure from the broker's store timestamp,
// which the replication loop hands the applier as an argument, because a
// field a writer could set is one it could lie about. A NOTIFY BLOCK: an
// identity change is no agent's work, and the contact updates a status change
// needs are derived by a duty from the rows. AN IP ADDRESS, USER AGENT OR
// DEVICE FINGERPRINT: request-scoped, and on this permanently retained,
// replicated log they would make every data node a location history.

// MaxReason bounds the reason a record carries: a line of prose, since the
// trail already headlines the op and the reason only says WHICH of the
// indistinguishable causes fired.
const MaxReason = 256

// ErrFutureVersion reports a record a newer build wrote. It carries the
// SUBJECT because the applier's deferral arm must file the record under one;
// without it the record could only be dropped.
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

// DecodeEnvelope is the FIRST pass, and it never fails on version: the subject
// an undecodable record is filed under, the scope a writer probes, the op a
// gate reads and the version deciding the second pass all come from here.
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

// Decode is the SECOND pass, and the one that may refuse on version. It
// returns the envelope beside the error whenever that decoded: the applier
// retains such a record by its subject, scope and op.
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

// Encode renders a record, stamping it with the lowest version that reads it
// and carrying back whatever a newer build wrote.
//
// A ZERO VERSION IS "STAMP IT": the minimum over [versionedFields] of the
// bytes about to be published, so the stamp cannot drift from the content. A
// set version is kept (a relay re-encodes at its writer's) but refused below
// what the record's fields need, since the builds the stamp holds back would
// apply it lossily. A [MutationRecord.readByEveryBuild] record carrying any
// versioned field is refused, save a gate's changed apply
// ([statelog.VersionedField.Gate]). LOSSLESS both ways: the reanchor path
// republishes records it half understood and must not strip the other half.
func Encode(rec MutationRecord) ([]byte, error) {
	return rec.encodeWith(versionedFields)
}

// encodeWith is [Encode] under a named field table — the seam the stamping
// rule is tested through with a field a later build would add.
func (rec MutationRecord) encodeWith(fields statelog.RecordFields) ([]byte, error) {
	stamp := rec.V == 0
	if stamp {
		// THE LOWEST, never [RecordVersion]: the table raises it to what
		// the record's own fields need.
		rec.V = BaseRecordVersion
	}
	data, err := jsoncarry.Encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: encode the record on %s: %w",
			rec.Subject, err)
	}
	// THE STAMP IS TAKEN OVER WHAT THIS BUILD WROTE, never what it CARRIES:
	// a carried field is not in this build's table, and a relay keeps its
	// writer's version.
	known := data
	if len(rec.Extra) > 0 {
		if known, err = jsoncarry.Encode(rec, nil); err != nil {
			return nil, fmt.Errorf("iamdomain: encode the record on %s: %w",
				rec.Subject, err)
		}
	}
	minimum, err := fields.Minimum(string(rec.Op), known)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: stamp the record on %s: %w", rec.Subject, err)
	}
	refused := fields.Carried(string(rec.Op), known)
	if rec.InstallsGate() {
		refused = fields.Ungated(string(rec.Op), known)
	}
	switch {
	case rec.readByEveryBuild() && minimum > BaseRecordVersion && len(refused) > 0:
		// See [MutationRecord.readByEveryBuild]: only a gate's changed
		// apply may carry a versioned field, at which an older node HALTS.
		return nil, fmt.Errorf("iamdomain: the %s record on %s must be readable "+
			"by every build for ever — an older node halts at a gate, and retains "+
			"a barrier or a generation, that it cannot read — and it carries %s, "+
			"so a field it needs belongs on a record that is not one of those",
			rec.Op, rec.Subject, strings.Join(refused, ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("iamdomain: encode the record on %s: %w",
				rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("iamdomain: the %s record on %s is stamped version "+
			"%d and carries %s — a build reading %d would decode it, drop what it "+
			"has no field for and apply the rest; leave the version unset and "+
			"the encoder stamps the lowest one that reads it", rec.Op, rec.Subject,
			rec.V, strings.Join(fields.Carried(string(rec.Op), known), ", "), rec.V)
	}
	return data, nil
}

// readByEveryBuild reports a record every build must be able to read, so it
// stays at [BaseRecordVersion] and carries no versioned field. A gate, because
// an undecodable one stops rather than defers (a deferred removal,
// invalidation or eviction has nothing to repair it), so its shape only grows
// by addition ([GateRecordVersion]). The barrier, the trimmed record and a
// generation, because an older node RETAINS what it cannot read: a
// linearizable read waiting for ever, anchors kept, a transition never made.
//
// THE ONE EXCEPTION is a gate's own APPLY changing
// ([statelog.VersionedField.Gate]): no older build can honour it, so the
// record keeps its version and older nodes halt there rather than diverge.
func (rec MutationRecord) readByEveryBuild() bool {
	return rec.InstallsGate() || rec.Op == OpBarrier || rec.Op == OpTrimmed ||
		rec.Op == OpGeneration
}

// recordFields is every top-level name this build writes, DERIVED from the
// struct ([jsoncarry.Names]) so omitempty names are listed too: a name missing
// here decodes into the struct AND is carried as unknown, so the next encode
// writes the stale carried copy back over what the caller set.
var recordFields = jsoncarry.Names(MutationRecord{})

// EncodeBarrier renders the framework's barrier append as one of this domain's
// own records. Declaring one gives the domain a read index and so
// `linearizable`, which "may this person act, as of now" needs: answered by a
// node that has not applied a revocation, it is the failure the epoch exists
// to prevent.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("iamdomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("iamdomain: a barrier carries no op id and this " +
			"one has one — an op id becomes a message id, and a duplicate ack " +
			"is served with no quorum round trip at all, which is the one thing " +
			"a read barrier must never be")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// THE BASE VERSION, never the ceiling: a barrier an older
			// peer deferred is a linearizable read on that peer that
			// waits for ever.
			V:       BaseRecordVersion,
			Subject: BarrierSubject(),
			Op:      OpBarrier,
			// RESOLVED FROM ITS KIND ([ScopeSet.Resolve]), so whatever a
			// peer wrote a barrier intersects nothing and no linearizable
			// read waits behind another; the root is stated only because
			// a scope must encode to something.
			Scope: RootScope(),
			Gen:   env.Gen,
		},
	})
}

// EncodeTrimmed renders the trim's trimmed record ([statelog.TrimmedEnvelope],
// ADR-0080) as one of this domain's records, split as the barrier is: the
// framework owns when one is appended, what it removes and its floor; the
// domain owns the record's shape.
//
// THE WRITER TRAVELS, unlike a barrier's, so the eviction gate can drop an
// evicted node's. NO OP ID: one becomes a message id and a ledger row nothing
// resolves. NO PERSON, so no removal's tombstone holds it back. The floor is
// CHECKED here at the write, not in [Subject.Validate], which the read path
// runs too: a refusal there stops the applier on every node, where the
// framework's apply faults instead.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("iamdomain: a %q envelope on %q is not a trimmed record",
			env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("iamdomain: a trimmed record carries no op id and " +
			"this one has one — an op id becomes a message id and a ledger row " +
			"nothing resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// THE BASE VERSION for ever, for the barrier's reason: a
			// trimmed record an older peer retained is every anchor it
			// names kept on that peer.
			V:       BaseRecordVersion,
			Subject: TrimmedSubject(floor),
			Op:      OpTrimmed,
			// RESOLVED FROM ITS KIND to the framework's own scope, as a
			// barrier's is ([ScopeSet.Resolve]); the root is stated
			// because a scope must encode to something.
			Scope:  RootScope(),
			Gen:    env.Gen,
			Writer: env.Writer,
		},
	})
}
