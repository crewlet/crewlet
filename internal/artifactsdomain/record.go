package artifactsdomain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/jsoncarry"
	"github.com/crewlet/crewlet/internal/statelog"
)

// RecordVersion is the record shape THIS BUILD can decode, never the version it
// stamps: [Encode] stamps the LOWEST version that reads a record, from
// [versionedFields], and this moves only with that table, to its highest
// version. A record above it is RETAINED, not skipped, so a rolling upgrade
// reduces coverage rather than causing an outage.
//
//   - 1: the base format.
//   - 2: the `folder` and `brand` subject kinds and their ops, an artifact's
//     `move`, a create's `folder`, a create of the `brand` artifact kind, and
//     a version's `brand` stylesheet.
//   - 3: the `comment` subject kind and its ops (ADR-0154), and `inbox_marks`.
const RecordVersion = 3

// baseRecordVersion is the base format, at which the barrier, the trimmed
// record, a reanchor's generation and every gate are pinned for ever: an older
// node retains what it cannot read, and a retained gate licenses every record
// above it (internal/doclet's copy).
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER. A field such a record needs that it cannot have belongs somewhere else.
const GateRecordVersion = 1

// versionedFields is every field gained since the base format, and the version
// a reader needs to apply a record carrying it. Adding one is a row here,
// [RecordVersion] moved, and a candidate in statelog_test.go's carrying.
//
// A NEW SUBJECT KIND OR OP IS A VALUE (every record has carried `subject.k`
// and `op` since the base format), so its row names the key AND the value, and
// an older build retains those records instead of faulting in dispatch; an op
// only a new kind carries needs no row (the kind's row stamps it). A NEW
// ARTIFACT KIND is a value at a create's `mutation.kind`: an older build
// refuses a kind it does not serve, so an unstamped create would diverge the
// fleet. Later records on the artifact carry no kind, so the create's row is
// the only one it needs.
var versionedFields = statelog.RecordFields{
	{Name: "Subject.Kind folder", Since: 2, Path: []string{"subject", "k"}, Equals: string(KindFolder)},
	{Name: "Subject.Kind brand", Since: 2, Path: []string{"subject", "k"}, Equals: string(KindBrand)},
	{Name: "RecordEnvelope.Op move", Since: 2, Path: []string{"op"}, Equals: string(OpMove)},
	{Name: "CreatePayload.Folder", Since: 2, Op: string(OpCreate), Path: []string{"mutation", "folder"}},
	{Name: "CreatePayload.Kind brand", Since: 2, Op: string(OpCreate), Path: []string{"mutation", "kind"},
		Equals: string(artifacts.KindBrand)},
	{Name: "VersionPayload.Brand", Since: 2, Path: []string{"mutation", "version", "brand"}},
	{Name: "Subject.Kind comment", Since: 3, Path: []string{"subject", "k"}, Equals: string(KindComment)},
	{Name: "RecordEnvelope.Op inbox_marks", Since: 3, Path: []string{"op"}, Equals: string(OpInboxMarks)},
}

// VersionedFields is the table, for the conformance suite.
func VersionedFields() statelog.RecordFields { return slices.Clone(versionedFields) }

// OpKind is what a record does.
type OpKind string

// The ops an artifacts record can carry.
const (
	// OpCreate makes an artifact and its first version in one record: on
	// the artifact's own subject when it has no source key, on the source
	// subject when it has one, and on an asset subject it adds an asset.
	OpCreate OpKind = "create"

	// OpPublish adds a version, decided against the head it names. Its
	// payload also names the unlabelled versions it retires.
	OpPublish OpKind = "publish"

	// OpRestoreVersion adds a version whose files are an earlier version's,
	// every entry carried. History is never rewritten.
	OpRestoreVersion OpKind = "restore_version"

	// OpPatch changes the head's title, description or icon, or one
	// version's label.
	OpPatch OpKind = "patch"

	// OpShare grants one audience a role, and OpUnshare withdraws it.
	OpShare   OpKind = "share"
	OpUnshare OpKind = "unshare"

	// OpTransfer moves ownership to another seat.
	OpTransfer OpKind = "transfer"

	// OpTrash stamps an artifact trashed and OpRestore clears the stamp: two
	// ops, not one with a direction, because the op is what both the applier
	// and an operator reading the log go by.
	OpTrash   OpKind = "trash"
	OpRestore OpKind = "restore"

	// OpPurgeBegin commits a trashed artifact to its purge, stamping when it
	// began: from then on a restore is refused and only a purge finishes it.
	// The datastore's removal, the first irreversible step, runs behind it.
	OpPurgeBegin OpKind = "purge_begin"

	// OpPurge removes an artifact permanently and INSTALLS A GATE by its op
	// rather than by its kind: its subject is an ordinary artifact.
	OpPurge OpKind = "purge"

	// OpRemove removes an asset, leaving its row as a tombstone that names
	// no object.
	OpRemove OpKind = "remove"

	// OpPin and OpUnpin keep and drop one person's pin on an artifact.
	OpPin   OpKind = "pin"
	OpUnpin OpKind = "unpin"

	// OpOpened marks when a person last opened an artifact: additive, and
	// monotone on broker time.
	OpOpened OpKind = "opened"

	// OpLinkWorkItem links an existing artifact to a tracker work item. It
	// changes no artifact state: its one history row carries the item's key,
	// which [ListQuery.WorkItem] reads, as for one created for the item.
	OpLinkWorkItem OpKind = "link_work_item"

	// OpReadElevated records that the admin path opened an artifact for a
	// caller with no relation to it (D09), shown to its owner and editors in
	// its history.
	// Its operation id derives from the day, admin, artifact and version, so
	// a repeat within the day collapses.
	OpReadElevated OpKind = "read_elevated"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record that removes the anchors
	// below a floor (ADR-0080).
	OpTrimmed OpKind = "trimmed"

	// OpMove files an artifact in another folder (on its subject), or moves a
	// folder under another parent (on the tree subject). An artifact's move is
	// not an edit: `updated_at` stays, and it grants and withdraws nothing.
	OpMove OpKind = "move"

	// OpRename renames a folder, on the tree subject.
	OpRename OpKind = "rename"

	// OpAdopt adopts one version of a brand kit as the company's brand, on
	// the brand subject, and OpClear withdraws the adoption.
	OpAdopt OpKind = "adopt"
	OpClear OpKind = "clear"

	// The ops on a comment thread's subject (ADR-0154), named for the thread so
	// a log line says what happened without its subject beside it.
	OpCommentOpen    OpKind = "comment_open"
	OpCommentReply   OpKind = "comment_reply"
	OpCommentEdit    OpKind = "comment_edit"
	OpCommentRemove  OpKind = "comment_remove"
	OpCommentResolve OpKind = "comment_resolve"
	OpCommentReopen  OpKind = "comment_reopen"

	// OpInboxMarks moves a person's marks over their comment notices by one
	// gesture, on the person's own reader subject (marks.go).
	OpInboxMarks OpKind = "inbox_marks"
)

// OpKinds are every op, in the order they are documented. A FOLDER'S OPS
// REUSE THE ARTIFACT'S NAMES (create, remove, move): an op is dispatched with
// its subject kind, never alone.
var OpKinds = []OpKind{
	OpCreate, OpPublish, OpRestoreVersion, OpPatch, OpShare, OpUnshare, OpTransfer,
	OpTrash, OpRestore, OpPurgeBegin, OpPurge, OpRemove, OpPin, OpUnpin, OpOpened, OpLinkWorkItem,
	OpReadElevated, OpEviction, OpGeneration, OpBarrier, OpTrimmed, OpMove, OpRename,
	OpAdopt, OpClear, OpCommentOpen, OpCommentReply, OpCommentEdit, OpCommentRemove,
	OpCommentResolve, OpCommentReopen, OpInboxMarks,
}

// CommentOps are every op on a comment thread's subject, in the order they are
// documented.
var CommentOps = []OpKind{
	OpCommentOpen, OpCommentReply, OpCommentEdit, OpCommentRemove, OpCommentResolve,
	OpCommentReopen,
}

// HistoryOps are every op an artifact's history row is filed under, in the
// order they are documented: what the dashboard and the company trail go by.
//
// NOT [OpKinds]: a purge removes the history and writes none; pins and opened
// marks are one person's browsing; the log's own records name no artifact; an
// asset's records go under its own name ([historyOp]); folders and the brand
// have their own ([FolderHistoryOps], [BrandHistoryOps]); and a comment
// thread's own rows are its history. A move's row names folder ids only, never
// a name, so it reveals nothing of a folder the reader cannot see.
var HistoryOps = []OpKind{
	OpCreate, OpPublish, OpRestoreVersion, OpPatch, OpShare, OpUnshare, OpTransfer,
	OpTrash, OpRestore, OpPurgeBegin, historyOp(KindAsset, OpCreate), historyOp(KindAsset, OpRemove),
	OpLinkWorkItem, OpReadElevated, OpMove,
}

// FolderHistoryOps are every op a folder history row is filed under
// (`artifact_folder_history`).
var FolderHistoryOps = []OpKind{OpCreate, OpRename, OpMove, OpRemove}

// BrandHistoryOps are every op a brand history row is filed under
// (`artifact_brand_history`).
var BrandHistoryOps = []OpKind{OpAdopt, OpClear}

// Valid reports whether an op off the wire is one this build knows.
func (o OpKind) Valid() bool { return slices.Contains(OpKinds, o) }

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted. Its EIGHT
// keys are reserved at the top level for the format's life (a later version
// adds beside them, never repurposes one), and are internal/doclet's eight, so
// two logs read alike.
type RecordEnvelope struct {
	// V is the record version.
	V int `json:"v"`

	// OpID is an operation id in the state log's grammar: the idempotency key,
	// broker message id and ledger key. EMPTY on
	// a barrier, whose ack must never come from a dedupe cache, and on a
	// trimmed record, whose identical copies must each land.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates and the unit the broker
	// arbitrates it on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is the writer's clock, for an operator reading the log, and
	// NEVER written to a row: every stored instant is the broker's.
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
// envelope alone: an eviction by its kind, a purge by its op.
func (e RecordEnvelope) InstallsGate() bool {
	return e.Subject.Kind.InstallsGate() || e.Op == OpPurge
}

// readByEveryBuild reports a record every build must read: a gate, the
// barrier, the trimmed record and a reanchor's generation. None may carry a
// versioned field: an older node would retain it (a retained trimmed record
// keeps every anchor it names).
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

	// The actor, as iam.ActorFor records one: the BARE name (seat handle, or
	// an unbound machine's login), its kind and credential; never a display
	// name.
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

// ErrFutureVersion reports a record a newer build wrote. It carries the
// SUBJECT because the caller is the deferral arm, which files it under one.
type ErrFutureVersion struct {
	Got     int
	Want    int
	Subject Subject
}

func (e *ErrFutureVersion) Error() string {
	return fmt.Sprintf("artifactsdomain: the record on %s is version %d and this "+
		"build reads %d: it is RETAINED at its position rather than skipped, and "+
		"reprocessed by a build that knows the shape", e.Subject, e.Got, e.Want)
}

// DecodeEnvelope is the FIRST pass, and it never fails on version.
func DecodeEnvelope(payload []byte) (RecordEnvelope, error) {
	var env RecordEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return RecordEnvelope{}, fmt.Errorf("artifactsdomain: decode the record "+
			"envelope: %w", err)
	}
	if env.V <= 0 {
		return RecordEnvelope{}, fmt.Errorf("artifactsdomain: a record carries "+
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
		return MutationRecord{RecordEnvelope: env}, fmt.Errorf("artifactsdomain: "+
			"decode the record on %s: %w", env.Subject, err)
	}
	rec.Extra = extra
	return rec, nil
}

// Encode renders a record, carrying back whatever a newer build wrote, and
// stamps it with the lowest version that reads it. A ZERO VERSION MEANS "STAMP
// IT"; a set one (a relay's) is kept, and refused when below what the record
// carries. A record every build must read may carry no versioned field.
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
		return nil, fmt.Errorf("artifactsdomain: encode the record on %s: %w",
			rec.Subject, err)
	}
	minimum, err := fields.Minimum(string(rec.Op), data)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: stamp the record on %s: %w",
			rec.Subject, err)
	}
	switch {
	case rec.readByEveryBuild() && minimum > baseRecordVersion:
		return nil, fmt.Errorf("artifactsdomain: the %s record on %s must be "+
			"readable by every build for ever, and it carries %s, so a field it "+
			"needs belongs somewhere else", rec.Op, rec.Subject,
			strings.Join(fields.Carried(string(rec.Op), data), ", "))
	case rec.V >= minimum:
	case stamp:
		rec.V = minimum
		if data, err = jsoncarry.Encode(rec, rec.Extra); err != nil {
			return nil, fmt.Errorf("artifactsdomain: encode the record on %s: %w",
				rec.Subject, err)
		}
	default:
		return nil, fmt.Errorf("artifactsdomain: the %s record on %s is stamped "+
			"version %d and carries %s: a build reading %d would decode it, drop "+
			"what it has no field for and apply the rest; leave the version unset "+
			"and the encoder stamps the lowest one that reads it", rec.Op,
			rec.Subject, rec.V, strings.Join(fields.Carried(string(rec.Op), data), ", "),
			rec.V)
	}
	return data, nil
}

// recordFields is every top-level name this build writes, read off the
// struct's tags so no name is both decoded and carried as unknown.
var recordFields = jsoncarry.Names(MutationRecord{})
