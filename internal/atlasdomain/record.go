package atlasdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsoncarry"
	"github.com/crewlet/crewlet/internal/statelog"
)

// RecordVersion is the record shape THIS BUILD can decode, never the version
// it stamps: a writer stamps the lowest version that reads its record, computed
// by [Encode] from [versionedFields], and a record above this is retained.
//
//   - 1: the base format: a person's claim on an entry, the purge of an entry
//     or of a value, an hour's expiry, the trim's trimmed record, and the
//     framework's eviction, generation and barrier.
//   - 2: an integration's read of one entry (`read`, ADR-0129): the claims it
//     holds on the entry that changed, and the slots it withdraws.
//   - 3: an agent's claim (ADR-0132): a `claim` whose source is an agent, by
//     its seat, with the turn it was recorded in and the evidence the engine
//     checked behind it.
const RecordVersion = 3

// baseRecordVersion is version 1, the base format.
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries for
// ever: a node's eviction and a purge.
const GateRecordVersion = 1

// versionedFields is every field or kind an atlas record has gained since the
// base format, with the version a reader needs to apply a record carrying it. A
// new source of claims (an agent's, an integration's read) is a row naming its
// value at `mutation.source.kind`, and a new record kind a row naming the
// subject kind's value. A word the atlas vocabulary gains (a kind of entry, a
// fact, a relation) needs no row: the applier keeps every word as text and
// judges none, so it lands as the same row on every build, and a reader that
// does not know it yet skips the claim by name ([atlas.View.Skipped]).
//
// A KIND NO TAG HAS SHIPPED STILL GETS ITS ROW, for the access log's reason:
// two builds of a rolling upgrade disagree about it all the same, and a build
// without the row would fault on the record rather than retain it.
var versionedFields = statelog.RecordFields{
	{Name: "Op read", Since: 2, Path: []string{"op"}, Equals: string(OpRead)},
	{Name: "Claim.Source agent", Since: 3, Op: string(OpClaim), Path: []string{"mutation", "source", "kind"},
		Equals: string(atlas.SourceAgent)},
}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

// The ops an atlas record can carry.
const (
	// OpClaim records one claim on an entry: a fact, a relation, the entry
	// itself, two entries joined or kept apart, or a denial of any of them;
	// a person's, or an agent's with the evidence of its turn.
	OpClaim OpKind = "claim"

	// OpRead records one integration's read of one entry (ADR-0129): the
	// claims the read holds on it that are new or changed, and the slots of
	// its own earlier claims it no longer holds, which are withdrawn. A read
	// that stopped seeing the entry retires it: one denied exists claim, and
	// every other slot withdrawn.
	OpRead OpKind = "read"

	// OpPurge erases an entry or a value everywhere, its history included,
	// and marks it so nothing brings it back.
	OpPurge OpKind = "purge"

	// OpExpire removes the history past the retention through an hour: the
	// claims a newer claim of the same voice replaced, and the entries a
	// person retired, with everything recorded about them.
	OpExpire OpKind = "expire"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record (ADR-0080).
	OpTrimmed OpKind = "trimmed"
)

// OpKinds are every op, in the order they are documented.
var OpKinds = []OpKind{
	OpClaim, OpRead, OpPurge, OpExpire, OpEviction, OpGeneration, OpBarrier, OpTrimmed,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at every version, before V is consulted. Its eight
// keys are every strict domain's eight and are reserved for the format's life.
type RecordEnvelope struct {
	// V is the record version.
	V int `json:"v"`

	// OpID is the idempotency key, the broker's message id and the ledger's key.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates and arbitrates on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is the writer's own clock, never written to a row: every
	// instant a row records is the broker's.
	CreatedAt time.Time `json:"created_at,omitzero"`

	// Gen is the generation the writer published in.
	Gen uint32 `json:"gen,omitempty"`

	// Writer is the publishing node's id, read by the eviction gate.
	Writer string `json:"writer,omitempty"`

	// Scope is the complete set of objects this record's apply may write.
	Scope ScopeSet `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate: an eviction
// by its kind, a purge by its op.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
}

// readByEveryBuild reports a record every build must read: a gate, the
// barrier, the trimmed record and a generation. None may carry a versioned field.
func (e RecordEnvelope) readByEveryBuild() bool {
	return e.InstallsGate() || e.Op == OpBarrier || e.Op == OpTrimmed || e.Op == OpGeneration
}

// MutationRecord is one committed mutation: the envelope, the typed payload as
// bytes, and who wrote it.
type MutationRecord struct {
	RecordEnvelope

	// Mutation is the typed payload for (Subject.Kind, Op), left as opaque
	// bytes when the version is above this build's.
	Mutation json.RawMessage `json:"mutation,omitempty"`

	// The actor, as iam.ActorFor records a principal.
	Actor      string        `json:"actor,omitempty"`
	ActorKind  iam.ActorKind `json:"actor_kind,omitempty"`
	OperatorID string        `json:"operator_id,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// actor is who wrote the record, as the record names them.
func (r MutationRecord) actor() iam.Actor {
	return iam.Actor{Name: r.Actor, Kind: r.ActorKind, OperatorID: r.OperatorID}
}

// ErrFutureVersion reports a record a newer build wrote, naming its subject
// for the deferral arm that files it.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("atlasdomain: the record on %s is version %d and this build reads %d: "+
		"it is retained at its position and reprocessed by a build that knows the shape",
		e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the first pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("atlasdomain: decode the record envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("atlasdomain: a record carries version %d: every "+
			"record states its version", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the second pass, and it may refuse on version. It returns the
// envelope beside the error whenever the envelope decoded, for the deferral.
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
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("atlasdomain: decode the "+
			"record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote, and
// stamps it with the lowest version that reads it. A zero version is "stamp
// it"; a set one is kept, and refused below what the record carries.
func Encode(rec MutationRecord) ([]byte, error) { return encodeWith(rec, versionedFields) }

// encodeWith is [Encode] under a named field table.
func encodeWith(rec MutationRecord, fields statelog.RecordFields) ([]byte, error) {
	stamp := rec.V == 0
	if stamp {
		rec.V = baseRecordVersion
	}
	data, err := jsoncarry.Encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("atlasdomain: encode the record on %s: %w", rec.Subject, err)
	}
	minimum, err := fields.Minimum(string(rec.Op), data)
	if err != nil {
		return nil, fmt.Errorf("atlasdomain: stamp the record on %s: %w", rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > baseRecordVersion:
		return nil, fmt.Errorf("atlasdomain: the %s record on %s must be readable by every "+
			"build and it carries %s", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), data), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("atlasdomain: encode the record on %s: %w", rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("atlasdomain: the %s record on %s is stamped version %d and "+
			"carries %s; leave the version unset and the encoder stamps the lowest that reads it",
			rec.Op, rec.Subject, rec.V, strings.Join(fields.Carried(string(rec.Op), data), ", "))
	}
	return data, nil
}

// recordFields is every top-level name this build writes, from the tags.
var recordFields = jsoncarry.Names(MutationRecord{})
