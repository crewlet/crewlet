package executordomain

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
//   - 1: the base format: a capture, an end, a retention delete, a seat's
//     removal, the trim's trimmed record, and the framework's eviction,
//     generation and barrier.
//   - 2: a turn's `begin` ([Begin]: its first read of its conversation and the
//     base that read froze, which the row then holds), and a capture's
//     context and cut-off ([Capture.ContextTokens], [Capture.Interrupted]).
//   - 3: an end's sessions that ran out of context
//     ([executorkeep.Ending.Exhausted]), which its apply retires.
//   - 4: an end that says its turn went on past the session its row names
//     ([executorkeep.Ending.WentOn]), and the rule an end stamped 4 or above is
//     applied by: it never takes the row's session off for an unkept attempt
//     ([wentOnVersion]).
const RecordVersion = 4

// baseRecordVersion is version 1, the base format.
const baseRecordVersion = 1

// wentOnVersion is the version an end that says its turn went on past its
// row's session is stamped at, and the version from which an end is APPLIED
// BY THE RULE THAT CAME WITH IT ([applyEnd]): the row keeps its session
// whatever unkept attempt the end names beside it, and the end absorbs and
// dedupes as any turn that landed a capture does. An end stamped below it
// keeps the rule it was written under — the row's older session released for
// an unkept attempt newer than it, and nothing absorbed — because a record's
// meaning is fixed by its stamp: a node of the build before reads every end it
// can at its own rule, so a node of this build applying those ends by another
// would hold rows no other copy holds. That is no reading of an older build's
// data; it keeps the estate a pure function of the log.
const wentOnVersion = 4

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER: a node's eviction, the one gate here. A field such a record needs that
// it cannot have belongs somewhere else.
const GateRecordVersion = 1

// versionedFields is every field an executor record has gained since the base
// format, and the version a reader must be at to apply a record carrying it.
// Adding one is a row here, [RecordVersion] moved to its version, and a
// conformance candidate that carries it (statelog_test.go's carrying).
//
// A FIELD NO TAG HAS SHIPPED STILL GETS ITS ROW: two builds of a rolling
// upgrade disagree about it all the same. A build at version 1 has no applier
// for a `begin` — its dispatch would fault on the (conversation, begin) pair —
// and no column for a capture's context or cut-off, which it would decode
// around, apply without, and so hold rows that differ from every upgraded
// node's for as long as the turn's row lives. With the rows it retains both,
// and applies them once it can.
//
// A NEW OP IS A VALUE of a key every record has carried since the base format,
// so its row names the key and the value: a row naming `op` alone would stamp
// every record the domain writes. A capture's two fields are omitted at their
// zero, so a capture that recorded neither stays readable by every build. An end
// names exhausted sessions only where one ran out, and a build that applied it
// without reading them would keep rows no other node keeps. And an end says its
// turn went on past its row's session only where it did, which a build applying
// it without reading it would answer by releasing that session where every
// node of this build keeps it.
var versionedFields = statelog.RecordFields{
	{Name: "RecordEnvelope.Op begin", Since: 2, Path: []string{"op"}, Equals: string(OpBegin)},
	{Name: "Capture.ContextTokens", Since: 2, Op: string(OpCapture), Path: []string{"mutation", "context_tokens"}},
	{Name: "Capture.Interrupted", Since: 2, Op: string(OpCapture), Path: []string{"mutation", "interrupted"}},
	{Name: "End.Ending.Exhausted", Since: 3, Op: string(OpEnd), Path: []string{"mutation", "ending", "exhausted"}},
	{Name: "End.Ending.WentOn", Since: wentOnVersion, Op: string(OpEnd),
		Path: []string{"mutation", "ending", "went_on"}},
}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

// The ops an executor record can carry.
const (
	// OpBegin records a turn's first read of its conversation and the base
	// that read froze, which the turn's row then holds against every
	// release but a retirement while the turn keeps no session of its own,
	// on its conversation's subject.
	OpBegin OpKind = "begin"

	// OpCapture names a new object for a live turn's session, on its
	// conversation's subject.
	OpCapture OpKind = "capture"

	// OpEnd stamps how a turn ended and tombstones it, on its
	// conversation's subject: terminal.
	OpEnd OpKind = "end"

	// OpDelete removes one turn's row and tombstone past the retention, on
	// its conversation's subject, or a removed seat's record of its removal
	// past the retention, on the seat's subject.
	OpDelete OpKind = "delete"

	// OpSeatDelete removes every row a removed seat holds, on the seat's
	// subject.
	OpSeatDelete OpKind = "seat_delete"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record that removes the anchors
	// below a floor (ADR-0080).
	OpTrimmed OpKind = "trimmed"
)

// OpKinds are every op, in the order they are documented.
var OpKinds = []OpKind{
	OpBegin, OpCapture, OpEnd, OpDelete, OpSeatDelete, OpEviction, OpGeneration, OpBarrier,
	OpTrimmed,
}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are reserved at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. They
// are every strict domain's eight, so an operator reading two logs reads one
// shape.
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
// envelope alone: a node's eviction, by its kind.
func (e RecordEnvelope) InstallsGate() bool { return e.Subject.Kind.InstallsGate() }

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

	// The actor, as iam.ActorFor records a principal: the name BARE, its
	// kind, and the credential it acted through. The engine writes every
	// record here on nobody's behalf but its own, as `system`.
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
	return fmt.Sprintf("executordomain: the record on %s is version %d and this "+
		"build reads %d: it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("executordomain: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("executordomain: a record carries "+
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
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("executordomain: "+
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
		return nil, fmt.Errorf("executordomain: encode the record on %s: %w",
			rec.Subject, err)
	}
	minimum, err := fields.Minimum(string(rec.Op), data)
	if err != nil {
		return nil, fmt.Errorf("executordomain: stamp the record on %s: %w",
			rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > baseRecordVersion:
		return nil, fmt.Errorf("executordomain: the %s record on %s must be "+
			"readable by every build for ever, and it carries %s, so a field it "+
			"needs belongs somewhere else", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), data), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("executordomain: encode the record on %s: %w",
				rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("executordomain: the %s record on %s is stamped "+
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
