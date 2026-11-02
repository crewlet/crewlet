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
//   - 1: every shape the base format has.
//   - 2: the folder tree and the adopted brand: the `folder` and `brand`
//     subject kinds and every op on them, an artifact's `move`, the `folder`
//     a create files into, a create of the `brand` artifact kind (a brand
//     kit), and the `brand` stylesheet a version pins.
const RecordVersion = 2

// baseRecordVersion is version 1, the base format. The barrier, the trim's
// trimmed record, a reanchor's generation and every gate are pinned at it for
// ever, for the reason
// internal/doclet states at its own copy: an older node retains what it cannot
// read, and a retained gate licenses every record above it.
const baseRecordVersion = 1

// GateRecordVersion is the version every gate-installing record carries, FOR
// EVER. A field such a record needs that it cannot have belongs somewhere else.
const GateRecordVersion = 1

// versionedFields is every field an artifacts record has gained since the base
// format, and the version a reader must be at to apply a record carrying it.
// Adding one is a row here, [RecordVersion] moved to its version, and a
// conformance candidate that carries it (statelog_test.go's carrying).
//
// A NEW KIND OR A NEW OP IS A VALUE, not a key: every record has carried
// `subject.k` and `op` since the base format, so its row names the key AND the
// value, and an older build retains the kind's records instead of faulting at
// the end of its dispatch. An op a new kind carries (a folder's `rename`, the
// brand's `adopt`) needs no row of its own: the kind's row already stamps
// every record on that subject.
//
// A NEW ARTIFACT KIND IS A VALUE TOO, at a create's `mutation.kind`: an older
// build's create apply refuses a kind it does not serve, so a node at the old
// version would refuse the very create a newer one applies, and the fleet
// would diverge. Its row stamps the create, which the older build then
// retains. Later records on that artifact carry no kind, so the create's row
// is the one row the kind needs.
var versionedFields = statelog.RecordFields{
	{Name: "Subject.Kind folder", Since: 2, Path: []string{"subject", "k"}, Equals: string(KindFolder)},
	{Name: "Subject.Kind brand", Since: 2, Path: []string{"subject", "k"}, Equals: string(KindBrand)},
	{Name: "RecordEnvelope.Op move", Since: 2, Path: []string{"op"}, Equals: string(OpMove)},
	{Name: "CreatePayload.Folder", Since: 2, Op: string(OpCreate), Path: []string{"mutation", "folder"}},
	{Name: "CreatePayload.Kind brand", Since: 2, Op: string(OpCreate), Path: []string{"mutation", "kind"},
		Equals: string(artifacts.KindBrand)},
	{Name: "VersionPayload.Brand", Since: 2, Path: []string{"mutation", "version", "brand"}},
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

	// OpTrash stamps an artifact trashed and OpRestore clears the stamp.
	// Two ops rather than one carrying a direction, because the op is both
	// what the applier dispatches on and what an operator reads in the log.
	OpTrash   OpKind = "trash"
	OpRestore OpKind = "restore"

	// OpPurgeBegin commits an artifact to its purge before anything the
	// purge removes is gone: it stamps the artifact in the trash with the
	// instant its purge began, and from then on a restore is refused and
	// only a purge finishes it. The datastore's removal, the purge's first
	// irreversible step, runs only behind it.
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

	// OpLinkWorkItem links an artifact that already exists to a tracker
	// work item. It changes no artifact state: it writes one history row
	// carrying the item's key, which is what a work item's listing reads
	// ([ListQuery.WorkItem]), so the artifact appears on that item exactly
	// as one created or published for it does.
	OpLinkWorkItem OpKind = "link_work_item"

	// OpReadElevated records that the admin path opened an artifact for a
	// caller with no relation to it (D09), which the artifact's own history
	// shows its owner and editors. Additive: its operation id is derived
	// from the day, the admin, the artifact and the version, so a repeat
	// within the day collapses.
	OpReadElevated OpKind = "read_elevated"

	// OpEviction is a node's eviction from this log, or its readmission.
	OpEviction OpKind = "eviction"

	// OpGeneration is a reanchor's record.
	OpGeneration OpKind = "generation"

	// OpBarrier is the read index's payload-free append.
	OpBarrier OpKind = "barrier"

	// OpTrimmed is the trim's payload-free record that removes the anchors
	// below a floor (ADR-0073).
	OpTrimmed OpKind = "trimmed"

	// OpMove files an artifact in another folder (on the artifact's
	// subject), or moves a folder under another parent (on the tree
	// subject). An artifact's move is not an edit: it leaves `updated_at`
	// alone and grants and withdraws nothing.
	OpMove OpKind = "move"

	// OpRename renames a folder, on the tree subject.
	OpRename OpKind = "rename"

	// OpAdopt adopts one version of a brand kit as the company's brand, on
	// the brand subject, and OpClear withdraws the adoption.
	OpAdopt OpKind = "adopt"
	OpClear OpKind = "clear"
)

// OpKinds are every op, in the order they are documented.
//
// A FOLDER'S OPS REUSE THE ARTIFACT'S NAMES where the gesture is the same
// (create, remove, move): the op is dispatched on the subject
// kind beside it, never alone.
var OpKinds = []OpKind{
	OpCreate, OpPublish, OpRestoreVersion, OpPatch, OpShare, OpUnshare, OpTransfer,
	OpTrash, OpRestore, OpPurgeBegin, OpPurge, OpRemove, OpPin, OpUnpin, OpOpened, OpLinkWorkItem,
	OpReadElevated, OpEviction, OpGeneration, OpBarrier, OpTrimmed, OpMove, OpRename,
	OpAdopt, OpClear,
}

// HistoryOps are every op an artifact's history row is filed under, in the
// order they are documented: what the dashboard words a history row by, and
// what the company trail narrows to.
//
// NOT [OpKinds]. A purge removes the history with the artifact and writes no
// row of its own; a pin, an unpin and an opened mark are one person's
// browsing rather than something that happened to the artifact; the log's own
// records are about no artifact; and an asset's records are filed under the
// asset's own name ([historyOp]), so the history tells an asset's create from
// the artifact's. A folder's records and the brand's are filed in their own
// histories ([FolderHistoryOps], [BrandHistoryOps]). An artifact's move is
// filed here, naming folder ids only, never a folder's name, so the row tells
// a reader of the artifact nothing about a folder they cannot see.
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

// RecordEnvelope decodes at EVERY version, BEFORE V is consulted.
//
// Its EIGHT keys are reserved at the top level of the format for its life: a
// later version may add fields beside them and may never repurpose one. They
// are internal/doclet's eight, so an operator reading two logs reads one shape.
type RecordEnvelope struct {
	// V is the record version.
	V int `json:"v"`

	// OpID is an operation id in the state log's grammar: the idempotency
	// key, the broker's message id and the ledger's key. EMPTY on a
	// barrier, whose acknowledgement must never come out of a dedupe cache,
	// and on a trimmed record, whose identical copies are each meant to land.
	OpID string `json:"op_id,omitempty"`

	// Subject is the object this record mutates and the unit the broker
	// arbitrates it on.
	Subject Subject `json:"subject"`

	// Op is what the record does.
	Op OpKind `json:"op"`

	// CreatedAt is the writer's own clock. It rides the record for an
	// operator reading the log and is NEVER written to a row: every instant
	// this domain stores is the broker's.
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
// barrier, the trim's trimmed record and a reanchor's generation. None may
// carry a versioned field: an older node retains what it cannot read, and a
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

	// The actor, as iam.ActorFor records a principal: the name BARE (a seat
	// handle, or a machine login for an unbound machine), its kind, and
	// the credential it acted through. Never a display name.
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
// struct's own tags so a name missing here is not decoded into the struct AND
// carried as unknown.
var recordFields = jsoncarry.Names(MutationRecord{})
