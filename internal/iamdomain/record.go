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

// RecordVersion is the record shape THIS BUILD can decode.
//
// A record above it is RETAINED rather than skipped — see the deferral contract
// in [statelog] — which is what makes a rolling upgrade a period of reduced
// coverage rather than an outage.
//
// IT IS THE CEILING THIS BUILD READS, NOT WHAT IT WRITES. Every record is
// stamped by [Encode] with the lowest version that reads what it carries, over
// [versionedFields], because a record a peer cannot read is deferred on that
// peer: written at the ceiling for no reason, every record of a rolling upgrade
// would be deferred on every older node. It moves only with that table, and
// exactly to its highest version. Two is [SweepRecordVersion], three is
// [OperatorRecordVersion], four is [ConditionRecordVersion], and nothing else
// has moved.
const RecordVersion = 4

// BaseRecordVersion is what every record carries whose meaning has not changed
// since this domain landed: every op but a sweep, written by a party that
// acted through no credential of its own.
const BaseRecordVersion = 1

// SweepRecordVersion is what a sweep record carries.
//
// VERSION 2 COLLECTS MORE: beside what version 1 does, the credentials and
// the redeemed invitations that stopped being presentable before the record's
// collection instant ([Writer.Sweep]).
//
// A VERSION AND NOT A FIELD, because a sweep deletes by a predicate every node
// evaluates for itself. A clause an older build does not know would be carried
// past as an unknown field and evaluated nowhere on that node, so the same
// record would delete rows on some nodes and not on others — the estate's
// byte-identical copies disagreeing for as long as the rollout lasts and after
// it, since a record is never applied twice. At version 2 an older node
// DEFERS the record and applies it once it is upgraded, under the semantics it
// was written with; and a version-1 sweep already on the log is still applied
// as version 1, because replay is the one reader that meets both. The record
// says which predicate it states with a marker ([MutationRecord.CollectsSpent])
// whose row in [versionedFields] is what stamps it.
const SweepRecordVersion = 2

// OperatorRecordVersion is what a record carries that names the credential its
// actor acted THROUGH ([MutationRecord.OperatorID]).
//
// VERSION 3 NAMES THE CREDENTIAL, so the trail row a gesture writes tells a
// machine token's gesture, or a browser session's, from the person's own —
// where it used to record the owner alone, and `GET /iam/audit` showed
// whatever somebody's token did to the directory as done by them.
//
// A VERSION AND NOT JUST A FIELD, for [SweepRecordVersion]'s reason one table
// over. The trail row is in this domain's identity claim, and an older build
// cannot write the field it does not know: it has no column to put it in, and
// it carries the field into the row's document in a different key order from a
// build that knows it. The same record would leave two different rows on two
// builds, and a record is never applied twice, so the copies would stay
// different after the upgrade. At version 3 an older node DEFERS the record,
// holds back what follows it in its bucket, and applies it once upgraded.
//
// ONLY A RECORD THAT HAS A CREDENTIAL TO NAME, which is the lowest-version rule
// again: the node's own writer acts through none — every sign-in, sign-out,
// step-up and enrolment the sign-in surface writes, and every duty — so those
// stay at the base, and what a rolling upgrade defers is the administrative
// gestures somebody made through `/iam`. See [namesOperator] for which records
// carry one.
const OperatorRecordVersion = 3

// ConditionRecordVersion is what a record carries that states a CONDITION on
// what it permits — a condition an older build does not know, would carry past
// as an unknown field, and would therefore apply as permitting MORE than its
// writer said.
//
// Two records state one:
//
//   - a SESSION START that may only enrol a second factor
//     ([Session.EnrolmentOnly]): an older node would apply it as a whole
//     session, and its rows — its session listing, and anything that reads
//     the row — would say the session may do everything. The bearer carries
//     the restriction too, in a form an older build refuses outright
//     ([session.Bearer.EnrolmentOnly]); the version is what keeps that
//     build's ROWS from saying the opposite.
//   - an INVITATION that is redeemable only with its link's secret and that
//     binds a seat when it is redeemed ([Invitation.Verifier],
//     [Invitation.Seat]): an older node would redeem it on its id alone, which
//     every snapshot and backup holds in the clear, and enrol the person with
//     no seat.
//
// A VERSION AND NOT JUST A FIELD, for [SweepRecordVersion]'s reason turned
// round: there a field an older build skipped made two nodes' rows differ,
// and here it would make one node's rows say a condition does not exist. At
// version 4 an older node DEFERS the record, and a deferred record is the
// unknown arm on every read of its bucket — a 503 on a restricted session or
// an invitation there, never the wider answer.
//
// ONLY THE RECORDS THAT CARRY A CONDITION: an ordinary session start stays at
// the base, or every sign-in of a rolling upgrade would be deferred on every
// older node. The versions are cumulative, so a version-4 record that also
// names a credential needs nothing more.
const ConditionRecordVersion = 4

// versionedFields is every field an identity record has gained since the base
// format, and the version a reader must be at to apply a record carrying it —
// the one statement of which record travels at which version, read by [Encode]
// for every record a writer leaves unstamped.
//
// # Adding a field to any record or payload is adding a row here
//
// A field an older build has no home for is decoded AROUND: the build reads the
// version, finds it readable, applies what it understands and drops the rest,
// so its rows differ from every upgraded node's for good, on tables the
// identity claim compares byte for byte — or, for a condition, say it does not
// exist. The row is what makes the encoder stamp a version that build retains
// instead. A new field takes the next version above [RecordVersion], which
// moves with it, and the domain's statelogtest candidate gains a record
// carrying it, which certifies the path is where the encoder writes it.
//
// # And a new predicate is a row too
//
// A sweep's version-2 clause is a rule the applier reads off the record's
// VERSION, with no field of its own to carry it, so the record states it with
// a marker on its root ([MutationRecord.CollectsSpent]) — the tracker's
// `keeps_place` precedent — and the marker's row stamps it.
var versionedFields = statelog.RecordFields{
	// THE SWEEP'S VERSION-2 PREDICATE, which collects what was spent as well
	// as what lapsed. A build reading 1 would delete less from the same
	// record than every upgraded node does. Scoped to the sweep, the one op
	// that collects anything.
	{Name: "MutationRecord.CollectsSpent", Since: SweepRecordVersion,
		Op: string(OpSweep), Path: []string{"collects_spent"}},
	// THE CREDENTIAL AN ACTOR ACTED THROUGH, at version 3. A build reading 2
	// has no trail column for it and carries it into the row's document
	// under another key order. Every op, because the trail row every
	// recorded op writes is where it lands; a gate never carries it
	// ([namesOperator]), which [Encode] refuses besides.
	{Name: "MutationRecord.OperatorID", Since: OperatorRecordVersion,
		Path: []string{operatorField}},
	// THE CONDITIONS, at version 4. A build reading 3 would apply a session
	// that may only enrol as a whole one, and redeem an invitation on its
	// id alone and bind no seat. Each scoped to the one op whose payload is
	// the document that carries it.
	{Name: "Session.EnrolmentOnly", Since: ConditionRecordVersion,
		Op: string(OpOpen), Path: []string{"mutation", "enrolment_only"}},
	{Name: "Invitation.Verifier", Since: ConditionRecordVersion,
		Op: string(OpInvite), Path: []string{"mutation", "verifier"}},
	{Name: "Invitation.Seat", Since: ConditionRecordVersion,
		Op: string(OpInvite), Path: []string{"mutation", "seat"}},
}

// VersionedFields is the table, for the conformance suite and for an operator
// surface that names why a record was held back.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// namesOperator reports whether a record of op carries the credential its actor
// acted through: every record that writes a trail row, except a gate.
//
// THE TRAIL IS WHAT THE FIELD IS FOR, so a record that writes no row carries
// none — a sweep, a barrier, an eviction and a generation are facts about the
// log, and a credential on one would be read by nothing while still deferring
// it on every older node.
//
// AND A GATE CARRIES NONE, because a gate is pinned at [GateRecordVersion] for
// ever and a field it cannot have belongs somewhere else: a removal's and an
// invalidation's trail rows name their actor alone, and the events announcing
// them — `iam_session_ended` (reason `person_removed`) and
// `iam_session_generation_bumped` — carry the credential.
func namesOperator(op OpKind) bool {
	if _, recorded := ClassOf(op); !recorded {
		return false
	}
	switch op {
	case OpRemove, OpEviction, OpInvalidate:
		return false
	}
	return true
}

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
//
// [OpInvalidate] is the third, and it is the removal's reason at the widest
// blast radius this domain has: a node that deferred it goes on honouring
// every session bearer in the company after somebody ended them all.
const GateRecordVersion = 1

// OpKind is what a record does.
type OpKind string

const (
	// OpInvite is an address spoken for by somebody who has no person yet,
	// on [KindDirectory], so it contends with every enrolment and every
	// other invitation: the decide refuses an address a person holds or
	// another open invitation holds.
	//
	// ITS OWN OP rather than a shape of [OpEnrol], because an invitation
	// names nobody: it holds an address open for a person who does not
	// exist, and what an operator reads in the log is the difference
	// between "this address was invited" and "this address belongs to
	// somebody".
	OpInvite OpKind = "invite"

	// OpEnrol creates a person WHOLE — their row, their first credentials,
	// their login, their address and their seat — and, for a redemption,
	// spends the invitation, in ONE record on [KindDirectory].
	//
	// ONE RECORD, because every half of it is a value the directory's
	// decide judges in one snapshot: the address, the login and the seat
	// nobody else holds, and the invitation not spent. Split, a half that
	// landed without the rest was a reservation every reader had to step
	// around and a sequence nothing could finish.
	OpEnrol OpKind = "enrol"

	// OpIdentity sets an enrolled person's LOGIN and SEAT binding, as
	// post-state, on [KindDirectory].
	//
	// BOTH ARE ALWAYS STATED, the unchanged one as the snapshot held it,
	// so a rename, a bind, an unbind and a move between seats are one
	// record each and the value the person gives up is freed by the same
	// record that takes the new one — there is no release step left to
	// not land.
	OpIdentity OpKind = "identity"

	// OpUpdate is a person's own content, as FULL POST-STATE: their name,
	// their contacts, their grants, their credential set.
	//
	// FULL POST-STATE AND NOT A PATCH, unlike the tracker: a person is
	// authored as a form and submitted whole, so the writer always holds the
	// complete new value and a patch would be a diff it computed in order to
	// be reassembled by every node.
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
	// Three things reach it and they are deliberately one op: a person
	// signing out everywhere, an administrator ending somebody's sessions,
	// and a second factor reset — each of which has to end whatever that
	// person's cookies are doing wherever they are.
	//
	// IT CARRIES A [MutationRecord.Reason] because the three are
	// indistinguishable afterwards and an operator investigating a
	// compromise needs to know which one fired.
	OpRevoke OpKind = "revoke"

	// OpRemove is a person the company no longer has, and it INSTALLS A
	// GATE by its op rather than by its kind: it rides the DIRECTORY
	// subject, because it frees a login, an address and a seat.
	//
	// The gate exists because a removal is the one operation here with no
	// inverse a later record supplies. Every other record is a full
	// post-state under a monotone guard, so a node that deferred one is
	// repaired by the next record about that person; nothing ever names a
	// removed person again, so a node that deferred a removal would go on
	// admitting somebody every peer has off-boarded.
	//
	// WHAT IT ERASES is every sealed value of theirs the estate holds —
	// their rows go, and the invitation and trail rows that outlive them
	// keep every column but the sealed ones. The tombstone and the id
	// outlive it, because the audit trail names them.
	OpRemove OpKind = "remove"

	// OpOpen begins one session. Its subject is [KindSession], create-only
	// at an expectation of zero, so a lineage can be opened exactly once.
	OpOpen OpKind = "open"

	// OpClose ends one session. Its subject is [KindSession].
	//
	// A RE-ISSUE IS NOT A RECORD and this is the reason the pair is only
	// two: the idle deadline a re-issue moves lives in the bearer's own
	// signed payload, so the busiest thing a signed-in person does writes
	// nothing on this log at all. What lands here is a session beginning
	// and a session ending, which is a handful of records per person per
	// day.
	OpClose OpKind = "close"

	// OpInvalidate ends EVERY session in the company at once, on
	// [KindInvalidation], by moving the fleet-wide generation a bearer
	// carries. It INSTALLS A GATE.
	//
	// THE GATE IS THE WHOLE POINT. A node that deferred this record would
	// go on honouring every cookie the company had just invalidated, and
	// there is no later record about any of those sessions to repair it —
	// which is the same shape of failure a deferred removal is, one
	// blast radius wider.
	//
	// IT IS NOT [OpRevoke] AT A LARGER SCALE, and the difference is which
	// counter moves: a revocation bumps one person's epoch and is rolled
	// back by a restore along with every other row, while this moves the
	// one number in the estate that can be pushed forward without knowing
	// who was affected. That is exactly what the restore runbook's last
	// step needs, because the sessions it has to end are the ones an
	// artefact taken before the revocation still believes in.
	//
	// THE NEW GENERATION IS STATED ON THE RECORD, never incremented by the
	// applier, for [OpRevoke]'s reason: an applier that did `+ 1` would
	// fold over an arrival order, and two nodes at one checkpoint have
	// seen the same set in a different order.
	OpInvalidate OpKind = "invalidate"

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
	OpInvite, OpEnrol, OpIdentity, OpUpdate, OpStatus, OpRevoke, OpRemove,
	OpOpen, OpClose, OpInvalidate, OpSweep, OpBarrier, OpEviction,
	OpGeneration,
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
// is an opaque id or names nobody, and the scope is a bucket number. A person's own
// values live in the payload, sealed under the fleet keyring, which the
// envelope pass never opens.
type RecordEnvelope struct {
	// V is the record version. Refused for the PAYLOAD when unknown, never
	// for this struct.
	V int `json:"v"`

	// OpID is the operation id, in the state log's grammar
	// (`<uuidv7>[.<name>][.<step>...]`, [statelog.NewOpID]): the
	// idempotency key, the Nats-Msg-Id, the ops table's key — and the
	// instant it was minted at, which the publisher reads to decide whether
	// this node's ledger can vouch for a retry. A step of a gesture carries
	// the gesture's id and its step ([statelog.StepOpID]). EMPTY on a
	// barrier, deliberately — an op id is what
	// invites a message id, and a duplicate ack is served out of the
	// dedupe window with no quorum round trip at all, which is the one
	// thing a read barrier must never be.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record arbitrates on.
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
	//
	// IT AND Gen ARE THE FRAMEWORK'S STAMP ([statelog.Stamp]), set from the
	// stamp a decide is handed — or, on the one record nothing decides, a
	// reanchor's, from the facts the transition hands [GenerationRecord] —
	// and from nowhere else: the publisher refuses a record that does not
	// carry the one it was decided under. Every
	// writer here left both empty until it did, so the eviction gate
	// compared an empty writer against every eviction on this log and
	// dropped nothing.
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
// ON THE OP AND NOT ON A KIND, because a removal rides the DIRECTORY subject
// beside every enrolment and identity change, so a reader that keyed on the
// kind would defer it — and a deferred removal here is a person the company
// off-boarded still signing in on one node. An eviction has a kind of its own and could have
// been read either way; it is read here so the question is asked once, in one
// place, for both — and so does an invalidation.
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
	// ONLY — the broker is what enforced it, and nothing reads this to
	// decide anything.
	Expect uint64 `json:"expect,omitempty"`

	// Mutation is the typed payload for (Subject.Kind, Op), left as OPAQUE
	// BYTES when the version is above this build's.
	//
	// IT IS WHERE EVERY SEALED VALUE LIVES. A person's name and address are
	// sealed under the fleet keyring BEFORE publication, with their id and
	// the field as the additional authenticated data, so the bytes on the
	// broker, in every node's deferred table and in every snapshot are
	// ciphertext that only a holder of the keyring can open.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	// Person is the id every row this record writes belongs to, IN THE
	// CLEAR, and it is here rather than in the envelope on purpose.
	//
	// The applier needs it to attach rows the subject does not name: a
	// directory record's subject names nobody and a session's names a
	// lineage, and the person is what their rows are keyed on. The
	// ENVELOPE does not carry it because
	// every envelope field is rendered by an operator surface for a record
	// nobody could decode, and a person's id in a stalled-log report is a
	// durable, replicated statement that this person exists.
	//
	// Empty on a barrier, a sweep, an eviction, a generation, an
	// invalidation and an invitation's issue — the records that are about
	// nobody; for a directory record about somebody it is that person.
	Person string `json:"person,omitempty"`

	// Actor and ActorKind are who did this.
	//
	// THE KIND IS A COLUMN AND NEVER A PREFIX ON THE NAME, which is
	// internal/iam's own rule: an operator prefixed into a name showed one
	// person as two people three rows apart in the audit feed.
	Actor     string   `json:"actor,omitempty"`
	ActorKind iam.Kind `json:"actor_kind,omitempty"`

	// OperatorID is the credential Actor acted THROUGH — a machine token's
	// `pat:<id>`, a browser session's `session:<lineage>`, a Tier A token's
	// own login — which is what tells a token's gesture from its owner's
	// when Actor names the owner either way. Empty where the party acted
	// through none, which is the node's own writer.
	//
	// ONLY AT [OperatorRecordVersion] AND ABOVE, and never on a gate: see
	// there, and [namesOperator]. [Decode] carries one found on a record
	// below that version exactly as a build that predates the field would,
	// rather than reading it.
	OperatorID string `json:"operator_id,omitempty"`

	// CollectsSpent says this sweep states version 2's predicate
	// ([SweepRecordVersion]): it collects the redeemed invitations and the
	// revoked or expired credentials that stopped being presentable before
	// its collection instant, beside what lapsed.
	//
	// A MARKER AND NOT A VALUE, read through the record's VERSION rather than
	// by the applier — the tracker's `keeps_place` precedent: the predicate
	// has no field of its own, so a sweep stamped by what it carries would be
	// version 1 and an older build would apply it with the smaller predicate.
	// Set by the writer on every sweep it builds ([Writer.record]); never on
	// any other op.
	CollectsSpent bool `json:"collects_spent,omitempty"`

	// Reason is why, in at most [MaxReason] bytes, for the operations
	// whose motive is not recoverable from their effect.
	//
	// A REVOCATION IS THE CASE IT EXISTS FOR: signing out everywhere, an
	// administrator ending somebody's sessions and a second factor reset
	// produce an identical epoch bump, and which one fired is the first
	// question anybody investigating a compromise asks. It is prose an operator wrote or a constant the
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
// A NOTIFY BLOCK. An identity change wakes nobody: a person being suspended, a
// session ending or a login being changed is not work for an agent, and the
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
	if rec.V < OperatorRecordVersion && rec.OperatorID != "" {
		// A FIELD IS READ AT THE VERSION THAT DEFINED IT. No writer puts
		// a credential on a record below [OperatorRecordVersion], and
		// one that did would be applied by a build that predates the
		// field — every node of the rolling upgrade that version exists
		// to protect — as an unknown key it carries. So this build does
		// exactly that, rather than writing a column those nodes do not
		// have and a document in a key order they do not write.
		var all map[string]json.RawMessage
		if err := json.Unmarshal(payload, &all); err != nil {
			return MutationRecord{RecordEnvelope: env}, fmt.Errorf("iamdomain: "+
				"decode the record on %s: %w", env.Subject, err)
		}
		if rec.Extra == nil {
			rec.Extra = map[string]json.RawMessage{}
		}
		rec.Extra[operatorField] = all[operatorField]
		rec.OperatorID = ""
	}
	return rec, nil
}

// operatorField is [MutationRecord.OperatorID]'s name on the wire.
const operatorField = "operator_id"

// Encode renders a record, stamping it with the lowest version that reads it
// and carrying back whatever a newer build wrote.
//
// A ZERO VERSION IS "STAMP IT": the writer leaves V unset on every record it
// builds and this computes the minimum over [versionedFields] from the bytes it
// is about to publish, so the stamp cannot drift from what the record carries.
// A version the caller DID set is kept — a gate is pinned at
// [GateRecordVersion], a barrier and a generation at [BaseRecordVersion], and a
// relay re-encodes a record at the version its writer gave it — but is refused
// when it is below what the record's own fields need, because that record
// would be applied, lossily, by exactly the builds the stamp exists to hold it
// back from. A record every build must read
// ([MutationRecord.readByEveryBuild]) is refused whenever it carries a
// versioned field at all.
//
// LOSSLESS IN BOTH DIRECTIONS, which is what makes a rolling upgrade safe: a
// node that read a record it only half understood and republished it — the
// reanchor path does exactly that — must not strip the half it did not.
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
	// THE STAMP IS TAKEN OVER WHAT THIS BUILD WROTE, never over what it
	// CARRIES: a carried field is one this build does not read, so it
	// cannot be what this build's table stamps — and a relay of a record
	// carrying one keeps the version its writer gave it.
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
	switch {
	case rec.readByEveryBuild() && minimum > BaseRecordVersion:
		// A GATE, A BARRIER OR A GENERATION NEVER CARRIES A VERSIONED
		// FIELD: a gate an older node cannot read is one it defers, and a
		// deferred removal is a person off-boarded still signing in on that
		// node; a barrier or a generation it cannot read is one it retains.
		return nil, fmt.Errorf("iamdomain: the %s record on %s must be readable "+
			"by every build for ever, and it carries %s — a field it needs "+
			"belongs on a record that is not a gate", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), known), ", "))
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

// readByEveryBuild reports a record every build there will ever be must be
// able to read: a gate, pinned at [GateRecordVersion] so an undecodable one is
// a stop rather than a deferral, and the read index's barrier and a reanchor's
// generation, pinned at [BaseRecordVersion] because an older node RETAINS a
// record it cannot read — a barrier is then a linearizable read on that node
// that waits for ever, and a generation a transition it never makes.
func (rec MutationRecord) readByEveryBuild() bool {
	return rec.InstallsGate() || rec.Op == OpBarrier || rec.Op == OpGeneration
}

// recordFields is every top-level name this build writes.
//
// DERIVED from the struct rather than typed again, on [jsoncarry.Names]'
// terms: the omitempty names have to be listed because a zero value does not
// marshal them, and a name missing here is decoded into the struct AND carried
// as unknown — so the next encode writes the stale carried copy back over what
// the caller set.
var recordFields = jsoncarry.Names(MutationRecord{})

// EncodeBarrier renders the framework's barrier append as one of this domain's
// own records.
//
// A DOMAIN THAT DECLARES ONE GETS A READ INDEX and therefore `linearizable`,
// and this domain needs one: "may this person act, as of now" is precisely a
// question about a position, and answering it from a node that has not applied
// a revocation is the failure the whole revocation epoch exists to prevent.
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
			// THE BARRIER'S SCOPE IS RESOLVED FROM ITS KIND rather than
			// from what is stated here — see [ScopeSet.Resolve] — so
			// whatever a peer wrote, a barrier read back on this build
			// intersects nothing and no linearizable read ever waits
			// behind another. The root is stated because a scope must
			// encode to something, and it is the one kind for which
			// that choice is not read.
			Scope: RootScope(),
			Gen:   env.Gen,
		},
	})
}
