package artifactdata

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
// contract). What a writer stamps is the LOWEST version that reads the record,
// computed by [Encode] from [versionedFields].
//
// # Why the version also pins the RULES
//
// The applier decides a write: its schema, field rules, guards and caps. Two
// builds that judged one record by different rules would give it two verdicts
// and their copies would part, so a change to how a record is judged (a new
// column type, a new op, a new predicate) is a field a record carries, a row in
// [versionedFields] and a version an older build defers rather than applies.
// The rules this build applies are the rules of version 1.
//
//   - 1: every shape this domain has.
const RecordVersion = 1

// baseRecordVersion is version 1, the base format.
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER: an older node retains what it cannot read, and a retained gate
// licenses every record above it.
const GateRecordVersion = 1

// versionedFields is every field a datastore record has gained since the base
// format. EMPTY.
var versionedFields = statelog.RecordFields{}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

const (
	// OpWrite is one batch of document ops, applied all or nothing.
	OpWrite OpKind = "write"

	// OpUndo restores what one earlier write changed, each document
	// conditioned on still being where that write left it.
	OpUndo OpKind = "undo"

	// OpConfigure replaces the store's collections, their rules and their
	// schemas, rewriting at most [artifacts.MaxMigrationDocs] documents.
	OpConfigure OpKind = "configure"

	// OpPurge removes the store permanently and INSTALLS A GATE by its op.
	OpPurge OpKind = "purge"

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

// OpKinds are every op.
var OpKinds = []OpKind{
	OpWrite, OpUndo, OpConfigure, OpPurge, OpEviction, OpGeneration, OpBarrier, OpTrimmed,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, before V is consulted. Its eight
// keys are the other state logs' eight, reserved for the format's life.
type RecordEnvelope struct {
	V         int       `json:"v"`
	OpID      string    `json:"op_id,omitempty"`
	Subject   Subject   `json:"subject"`
	Op        OpKind    `json:"op"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	Gen       uint32    `json:"gen,omitempty"`
	Writer    string    `json:"writer,omitempty"`
	Scope     ScopeSet  `json:"scope"`
}

// InstallsGate reports whether this record installs an apply gate, from the
// envelope alone: an eviction by its kind, a purge by its op.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
}

// readByEveryBuild reports a record every build must read: a gate, the
// barrier, the trim's trimmed record and a reanchor's generation. An older
// node retains what it cannot read, and a trimmed record it retained is every
// anchor it names kept on that node.
func (e RecordEnvelope) readByEveryBuild() bool {
	return e.InstallsGate() || e.Op == OpBarrier || e.Op == OpTrimmed ||
		e.Op == OpGeneration
}

// MutationRecord is one committed record: the envelope, the typed payload as
// bytes, and who wrote it.
type MutationRecord struct {
	RecordEnvelope

	Mutation json.RawMessage `json:"mutation,omitempty"`

	// The actor, as iam.ActorFor records a principal: the name bare (a seat
	// handle, or a machine login), its kind, and the credential it acted
	// through. Never a display name.
	Actor      string        `json:"actor,omitempty"`
	ActorKind  iam.ActorKind `json:"actor_kind,omitempty"`
	OperatorID string        `json:"operator_id,omitempty"`

	// TurnID is the agent turn that made the write. Provenance only.
	TurnID string `json:"turn_id,omitempty"`

	// Extra carries fields a newer build wrote, so a record round-trips
	// losslessly through a node that cannot interpret them.
	Extra map[string]json.RawMessage `json:"-"`
}

// actor is the record's author as the identity vocabulary names one.
func (r MutationRecord) actor() iam.Actor {
	return iam.Actor{Name: r.Actor, Kind: r.ActorKind, OperatorID: r.OperatorID}
}

// ErrFutureVersion reports a record a newer build wrote.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("artifactdata: the record on %s is version %d and this build "+
		"reads %d: it is RETAINED at its position rather than skipped, and reprocessed "+
		"by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("artifactdata: decode the record envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("artifactdata: a record carries version %d: "+
			"every record states its version", env.V)
	}
	if err := env.Subject.Validate(); err != nil {
		return RecordEnvelope{}, err
	}
	return env, nil
}

// Decode is the SECOND pass, and it may refuse on version. It returns the
// envelope alongside the error whenever the envelope itself decoded.
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
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("artifactdata: decode the "+
			"record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote, and
// stamps it with the lowest version that reads it. A zero version is "stamp
// it".
func Encode(rec MutationRecord) ([]byte, error) { return encodeWith(rec, versionedFields) }

// encodeWith is [Encode] under a named field table.
func encodeWith(rec MutationRecord, fields statelog.RecordFields) ([]byte, error) {
	stamp := rec.V == 0
	if stamp {
		rec.V = baseRecordVersion
	}
	data, err := jsoncarry.Encode(rec, rec.Extra)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: encode the record on %s: %w", rec.Subject, err)
	}
	minimum, err := fields.Minimum(string(rec.Op), data)
	if err != nil {
		return nil, fmt.Errorf("artifactdata: stamp the record on %s: %w", rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > baseRecordVersion:
		return nil, fmt.Errorf("artifactdata: the %s record on %s must be readable by every "+
			"build for ever, and it carries %s", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), data), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("artifactdata: encode the record on %s: %w", rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("artifactdata: the %s record on %s is stamped version %d and "+
			"carries %s; leave the version unset and the encoder stamps the lowest one "+
			"that reads it", rec.Op, rec.Subject, rec.V,
			strings.Join(fields.Carried(string(rec.Op), data), ", "))
	}
	return data, nil
}

// recordFields is every top-level name this build writes.
var recordFields = jsoncarry.Names(MutationRecord{})
