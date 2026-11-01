package assistantdomain

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

// RecordVersion is the record shape THIS BUILD can decode, never the version it
// stamps on what it writes.
//
// A record above it is RETAINED rather than skipped (the framework's deferral
// contract), which is what makes a rolling upgrade a period of reduced coverage
// rather than an outage. What a writer stamps is the LOWEST version that reads
// the record, computed by [Encode] from [versionedFields]. It moves ONLY with
// that table, and exactly to its highest version.
//
// # What each version added
//
//   - 1: every shape this domain has.
const RecordVersion = 1

// baseRecordVersion is version 1, the base format. A reanchor's generation and
// every gate are pinned at it for ever: an older node retains what it cannot
// read, and a retained gate licenses every record above it.
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER. A field such a record needs that it cannot have belongs somewhere else.
const GateRecordVersion = 1

// versionedFields is every field an assistant record has gained since the base
// format, and the version a reader must be at to apply a record carrying it.
// EMPTY: every field this build writes is in the base format. Adding one is a
// row here, [RecordVersion] moved to its version, and a conformance candidate
// that carries it.
var versionedFields = statelog.RecordFields{}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

// The ops an assistant record can carry.
const (
	// OpConversationCreated makes a conversation. Create-only on its own
	// subject.
	OpConversationCreated OpKind = "conversation_created"

	// OpConversationTitled sets a conversation's title.
	OpConversationTitled OpKind = "conversation_titled"

	// OpConversationPurged removes a conversation and everything in it for
	// good, and INSTALLS A GATE by its op: its subject is an ordinary
	// conversation.
	OpConversationPurged OpKind = "conversation_purged"

	// OpConversationAuditRead records an administrator's compliance read
	// (decision D06), beside the conversation so its owner sees it.
	// Additive: two reads never contend.
	OpConversationAuditRead OpKind = "conversation_audit_read"

	// OpFileStored records a file kept for a conversation (an image its
	// owner attached, or a file a deep-work run delivered), ON THE
	// CONVERSATION'S SUBJECT, because an attachment exists before the turn
	// it is sent with. Additive under an id the caller mints: a duplicate
	// publish writes one row, and two uploads never contend.
	OpFileStored OpKind = "file_stored"

	// OpTurnOpened opens a turn, ON THE CONVERSATION'S SUBJECT, so two
	// openings in one conversation contend and one wins: one running or
	// parked turn per conversation.
	OpTurnOpened OpKind = "turn_opened"

	// OpTurnParked parks a running turn on a proposal, with the loop's
	// conversation sealed beside it.
	OpTurnParked OpKind = "turn_parked"

	// OpTurnResumed hands a parked turn to a runner again.
	OpTurnResumed OpKind = "turn_resumed"

	// OpTurnClosed ends a turn in a final state and drops what it parked.
	OpTurnClosed OpKind = "turn_closed"

	// OpMessageRecorded, OpStepRecorded and OpCardRecorded record what the
	// person sent and was shown. Additive: their ids are derived from the
	// turn, so a duplicate publish writes the same row.
	OpMessageRecorded OpKind = "message_recorded"
	OpStepRecorded    OpKind = "step_recorded"
	OpCardRecorded    OpKind = "card_recorded"

	// OpProposalCreated makes a proposal. Create-only on its own subject.
	OpProposalCreated OpKind = "proposal_created"

	// The proposal's moves, each arbitrated on the proposal's subject and
	// conditioned on the state it was decided from ([ProposalMove]).
	OpProposalApproved  OpKind = "proposal_approved"
	OpProposalExecuting OpKind = "proposal_executing"
	OpProposalDeclined  OpKind = "proposal_declined"
	OpProposalExpired   OpKind = "proposal_expired"
	OpProposalStale     OpKind = "proposal_stale"
	OpProposalApplying  OpKind = "proposal_applying"
	OpProposalSettled   OpKind = "proposal_settled"
	OpProposalUnknown   OpKind = "proposal_unknown"
	OpProposalRetried   OpKind = "proposal_retried"

	// OpOwnerErased removes every conversation a person owns, for good, and
	// INSTALLS A GATE by its op: nothing is created for them afterwards.
	OpOwnerErased OpKind = "owner_erased"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record that removes the anchors
	// below a floor (ADR-0073).
	OpTrimmed OpKind = "trimmed"
)

// OpKinds are every op, in the order they are documented.
var OpKinds = []OpKind{
	OpConversationCreated, OpConversationTitled, OpConversationPurged,
	OpConversationAuditRead, OpFileStored, OpTurnOpened, OpTurnParked, OpTurnResumed, OpTurnClosed,
	OpMessageRecorded, OpStepRecorded, OpCardRecorded, OpProposalCreated,
	OpProposalApproved, OpProposalExecuting, OpProposalDeclined, OpProposalExpired,
	OpProposalStale, OpProposalApplying, OpProposalSettled, OpProposalUnknown,
	OpProposalRetried, OpOwnerErased, OpEviction, OpGeneration, OpBarrier, OpTrimmed,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// installsGate reports an op that installs a gate on an ordinary subject.
func (o OpKind) installsGate() bool { return o == OpConversationPurged || o == OpOwnerErased }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are reserved at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. They
// are internal/doclet's and internal/artifactsdomain's eight, so an operator
// reading two logs reads one shape.
type RecordEnvelope struct {
	// V is the record version.
	V int `json:"v"`

	// OpID is an operation id in the state log's grammar: the idempotency
	// key, the broker's message id and the ledger's key.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates and the unit the broker
	// arbitrates it on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is the writer's own clock. It rides the record for an
	// operator reading the log and is NEVER written to a row: every instant
	// a row records of its own is the broker's.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation the writer published in.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate before
	// any kind rule.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of objects this record's apply may write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate, from the
// envelope alone: an eviction by its kind, a purge and an erasure by their ops.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op.installsGate()
}

// readByEveryBuild reports a record every build must read: a gate, the
// barrier, the trim's trimmed record and a reanchor's generation. None may
// carry a versioned field. An older node retains what it cannot read, and a
// trimmed record it retained is every anchor it names kept on that node.
func (e RecordEnvelope) readByEveryBuild() bool {
	return e.InstallsGate() || e.Op == OpBarrier || e.Op == OpTrimmed ||
		e.Op == OpGeneration
}

// MutationRecord is one committed mutation: the envelope, the typed payload as
// bytes, and who wrote it.
type MutationRecord struct {
	RecordEnvelope

	// Mutation is the typed payload for (Subject.Kind, Op), left as OPAQUE
	// BYTES when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	// The actor, as iam.ActorFor records a principal: the name BARE (the
	// person's seat handle or login, or the engine), its kind, and the
	// credential it acted through. Never a display name.
	Actor      string        `json:"actor,omitempty"`
	ActorKind  iam.ActorKind `json:"actor_kind,omitempty"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// ErrFutureVersion reports a record a newer build wrote. It carries the
// SUBJECT because the caller is the deferral arm, which files it under one.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("assistantdomain: the record on %s is version %d and this "+
		"build reads %d: it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("assistantdomain: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("assistantdomain: a record carries "+
			"version %d: every record states its version, and one that does not "+
			"cannot be told apart from a newer build's", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the SECOND pass, and it may refuse on version. It returns the
// envelope alongside the error whenever the envelope itself decoded, because
// that is the case the applier retains.
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
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("assistantdomain: "+
			"decode the record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote, and
// stamps it with the lowest version that reads it.
//
// A ZERO VERSION IS "STAMP IT": every writer leaves V unset. A set version is
// kept (a relay keeps its writer's) and refused when it is below what the
// record carries. A record every build must read is refused whenever it
// carries a versioned field at all.
func Encode(rec MutationRecord) ([]byte, error) { return encodeWith(rec, versionedFields) }

// encodeWith is [Encode] under a named field table: the seam the stamping rule
// is tested through.
func encodeWith(rec MutationRecord, fields statelog.RecordFields) ([]byte, error) {
	stamp := rec.V == 0
	if stamp {
		rec.V = baseRecordVersion
	}
	data, err := jsoncarry.Encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: encode the record on %s: %w",
			rec.Subject, err)
	}
	minimum, err := fields.Minimum(string(rec.Op), data)
	if err != nil {
		return nil, fmt.Errorf("assistantdomain: stamp the record on %s: %w",
			rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > baseRecordVersion:
		return nil, fmt.Errorf("assistantdomain: the %s record on %s must be "+
			"readable by every build for ever, and it carries %s, so a field it "+
			"needs belongs somewhere else", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), data), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("assistantdomain: encode the record on %s: %w",
				rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("assistantdomain: the %s record on %s is stamped "+
			"version %d and carries %s: a build reading %d would decode it, drop "+
			"what it has no field for and apply the rest; leave the version unset "+
			"and the encoder stamps the lowest one that reads it", rec.Op,
			rec.Subject, rec.V, strings.Join(fields.Carried(string(rec.Op), data), ", "),
			rec.V)
	}
	return data, nil
}

// recordFields is every top-level name this build writes, read off the
// struct's own tags so a name missing here is not decoded into the struct AND
// carried as unknown.
var recordFields = jsoncarry.Names(MutationRecord{})
