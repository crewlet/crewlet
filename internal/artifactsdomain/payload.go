package artifactsdomain

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The typed payloads, one per (kind, op). A record carries its COMPLETE new
// values, never a rule for computing them, so a replay from zero rebuilds the
// same rows and the applier stays a pure function of record and rows.
//
// IDs travel as text, not [artifacts.ID]: an id a decoder refused would stall
// the log, where the applier parsing it refuses it deterministically as a row.

// VersionPayload is one version as a record states it: the first version of a
// create, a publish and a rollback all carry one.
type VersionPayload struct {
	// Number is head + 1, decided in the artifact subject's snapshot; the
	// broker's arbitration makes it unique.
	Number int `json:"number"`

	// VersionID is a name-based uuid over the record's operation, so a
	// retry names the version its first attempt made.
	VersionID string `json:"version_id"`

	// Base is the head decided against: Number - 1, even when forced (a
	// forced publish skips the caller's base check, not the arbitration).
	Base int `json:"base"`

	// Forced marks a publish a person with presence made over a moved head.
	Forced bool `json:"forced,omitempty"`

	// RestoredFrom is the version a rollback copies, every entry carried.
	RestoredFrom int `json:"restored_from,omitempty"`

	Label        string                 `json:"label,omitempty"`
	Contract     string                 `json:"contract,omitempty"`
	Capabilities []artifacts.Capability `json:"capabilities,omitempty"`

	// Manifest is the version's files in canonical form
	// (internal/artifacts/manifest), each naming its object.
	Manifest artifacts.Manifest `json:"manifest"`

	// Prune is the exact unlabelled versions this record retires, decided in
	// the writer's snapshot (package doc, "Retention rides the record").
	Prune []int `json:"prune,omitempty"`

	// SearchText is what a gallery search matches, at most
	// [artifacts.MaxSearchTextBytes].
	SearchText string `json:"search_text,omitempty"`

	// Brand is the adopted stylesheet a FRAMED version (html, design) is
	// published under: the generated CSS's SHA-256 in hex from the writer's
	// snapshot of the brand subject, empty for none (no brand, a native
	// kind, or `<html data-crewlet-brand="off">`). Since record version 2.
	Brand string `json:"brand,omitempty"`
}

// CreatePayload makes an artifact and its first version in one record.
//
// Its subject is the artifact, or the SOURCE address when there is a source
// key: the apply then writes claim, artifact, version and history in one
// transaction, and recomputes the subject from Owner and SourceKey, refusing
// a record that arbitrated on one address and claims another.
type CreatePayload struct {
	V int `json:"v"`

	// Artifact is the id, derived from the create's operation. On an
	// artifact subject it equals the subject's id.
	Artifact string `json:"artifact"`

	Kind        artifacts.Kind `json:"kind"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Icon        string         `json:"icon,omitempty"`

	// Owner is the seat handle (or machine login) that owns the artifact,
	// and OwnerKind its actor kind, which is what opens the lead arm (D12).
	Owner     string        `json:"owner"`
	OwnerKind iam.ActorKind `json:"owner_kind"`

	SourceKey string `json:"source_key,omitempty"`

	// Folder is where the artifact is created, empty for the top level,
	// checked live in the writer's snapshot and named in the scope
	// ([FolderTerm]) so the apply orders behind any record about it. Since
	// record version 2.
	Folder string `json:"folder,omitempty"`

	Version VersionPayload `json:"version"`

	// Shares are granted with the create (such as the requester an agent
	// published for, as editor). GrantedAt is the writer's clock and never
	// written: a row's instant is the broker's.
	Shares []artifacts.Share `json:"shares,omitempty"`

	// Authority is the decision stamped at the API; the applier enforces its
	// quota limits against the replicated counters.
	Authority artifacts.Authority `json:"authority"`
}

// PublishPayload adds a version: a publish, or a rollback with RestoredFrom set
// and every entry carried.
type PublishPayload struct {
	V         int                 `json:"v"`
	Version   VersionPayload      `json:"version"`
	Authority artifacts.Authority `json:"authority"`
}

// VersionLabel is one version's label as a patch sets it. An empty Label
// removes the version's label.
type VersionLabel struct {
	Number int    `json:"number"`
	Label  string `json:"label"`
}

// PatchPayload changes the head's displayed fields or one version's label. A
// nil field is unchanged.
type PatchPayload struct {
	V           int           `json:"v"`
	Title       *string       `json:"title,omitempty"`
	Description *string       `json:"description,omitempty"`
	Icon        *string       `json:"icon,omitempty"`
	Label       *VersionLabel `json:"label,omitempty"`

	// Prune is the versions a label's removal pushed past retention, retired
	// in this record so the bound holds after every write, not the next
	// publish.
	Prune []int `json:"prune,omitempty"`
}

// SharePayload grants one audience a role. A grant that LOWERS an existing
// role is a narrowing, and moves the artifact's access epoch.
type SharePayload struct {
	V     int             `json:"v"`
	Share artifacts.Share `json:"share"`
}

// UnsharePayload withdraws one audience's share.
type UnsharePayload struct {
	V        int                `json:"v"`
	Audience artifacts.Audience `json:"audience"`
}

// TransferPayload moves ownership. The previous owner's source address is
// released: an address is an owner's, and the new owner does not hold it.
type TransferPayload struct {
	V      int           `json:"v"`
	To     string        `json:"to"`
	ToKind iam.ActorKind `json:"to_kind"`
}

// LinkPayload links an existing artifact to one tracker work item, the
// authority's [artifacts.Authority.WorkItem] stamped at the API with who linked
// it and how, so its history row is filed under the item as a create made for
// it is.
type LinkPayload struct {
	V         int                 `json:"v"`
	Authority artifacts.Authority `json:"authority"`
}

// StatusPayload is a trash, a restore, a purge's beginning, a purge, or an
// asset's removal.
//
// A purge names its dependents' operations, the steps that first removed other
// domains' per-artifact state (the datastore), so the log shows the whole
// gesture.
type StatusPayload struct {
	V          int      `json:"v"`
	Dependents []string `json:"dependents,omitempty"`
}

// AssetPayload adds one asset. Never carried: its object is a fresh upload,
// named by no other row.
type AssetPayload struct {
	V           int                 `json:"v"`
	Name        string              `json:"name"`
	ContentType string              `json:"content_type"`
	Object      objstore.Object     `json:"object"`
	Authority   artifacts.Authority `json:"authority"`
}

// ReaderPayload is one person's pin, unpin, opened mark or elevated read of
// one artifact. Version is the version an elevated read opened.
type ReaderPayload struct {
	V        int    `json:"v"`
	Artifact string `json:"artifact"`
	Version  int    `json:"version,omitempty"`
}

// MovePayload files one artifact in another folder, on its own subject. From
// (as the writer's snapshot found it) and To are empty for the top level; the
// scope names each that is a folder. The apply refuses `folder_gone` for a To
// no longer live, and leaves `updated_at` alone: a move is not an edit.
type MovePayload struct {
	V         int                 `json:"v"`
	From      string              `json:"from,omitempty"`
	To        string              `json:"to,omitempty"`
	Authority artifacts.Authority `json:"authority"`
}

// FolderSpec is one folder a create makes.
type FolderSpec struct {
	// ID is a name-based uuid over the record's operation and the folder's
	// place in the chain ([FolderIDOf]), so a retry names the same folders.
	ID string `json:"id"`

	// Parent is the folder it sits in, empty for the top level: an existing
	// live folder, or the folder before it in the same record's chain.
	Parent string `json:"parent,omitempty"`

	// Name is the canonical name and NameKey its fold
	// ([foldername.Check]), both computed by the writer.
	Name    string `json:"name"`
	NameKey string `json:"name_key"`
}

// FolderCreatePayload makes one folder, or a chain of them (a path created
// with its missing parents in one record), on the tree subject.
type FolderCreatePayload struct {
	V int `json:"v"`

	// Folders is the chain, parents first. Every folder after the first
	// sits in the one before it.
	Folders []FolderSpec `json:"folders"`

	// Owner is the seat handle (or machine login) that owns the folders,
	// and OwnerKind its actor kind, which opens the lead arm.
	Owner     string        `json:"owner"`
	OwnerKind iam.ActorKind `json:"owner_kind"`

	// MaxDepth is the depth bound the writer decided under
	// ([artifacts.MaxFolderDepth]), stamped on every folder it makes.
	MaxDepth int `json:"max_depth"`

	// MaxFolders and MaxChildren bound the company's live folders and one
	// folder's live children ([artifacts.MaxFolders],
	// [artifacts.MaxFolderChildren]), STAMPED so the apply enforces the
	// writer's bound, not its own build's constant, which may differ.
	MaxFolders  int `json:"max_folders"`
	MaxChildren int `json:"max_children"`

	// Shares are a new TOP-LEVEL folder's visibility preset (the company as
	// contributor for "Everyone", none for "Only you"); empty for a
	// subfolder, which its top-level folder decides. No op changes them
	// later in this build.
	Shares []artifacts.Share `json:"shares,omitempty"`

	Authority artifacts.Authority `json:"authority"`
}

// FolderRenamePayload renames one folder.
type FolderRenamePayload struct {
	V         int                 `json:"v"`
	Folder    string              `json:"folder"`
	Name      string              `json:"name"`
	NameKey   string              `json:"name_key"`
	Authority artifacts.Authority `json:"authority"`
}

// FolderMovePayload moves one folder, and everything beneath it, under
// another parent (empty for the top level). The writer refuses a cycle and a
// depth past the stamp from its snapshot; the apply checks both again.
type FolderMovePayload struct {
	V      int    `json:"v"`
	Folder string `json:"folder"`
	Parent string `json:"parent,omitempty"`

	// Root is the top-level folder the moved folder sits under afterwards,
	// which every folder beneath it is re-rooted to in the same apply.
	Root string `json:"root"`

	// MaxDepth and MaxChildren are the depth bound and the bound on one
	// folder's live children the writer decided under, stamped for
	// [FolderCreatePayload.MaxFolders]' reason.
	MaxDepth    int `json:"max_depth"`
	MaxChildren int `json:"max_children"`

	// Shares are what a folder moved TO THE TOP LEVEL holds: its old
	// top-level folder's, so who sees its subtree is unchanged. Empty for a
	// move under a folder, which drops its own shares: its new top-level
	// folder decides from then on.
	Shares []artifacts.Share `json:"shares,omitempty"`

	Authority artifacts.Authority `json:"authority"`
}

// FolderRemovePayload removes one folder: its child folders and its artifacts
// (live and trashed) move up to Parent in the same apply, and its row becomes a
// tombstone. Its scope is the tree and every artifact ([TreeTerm],
// [ArtifactsTerm]).
type FolderRemovePayload struct {
	V         int                 `json:"v"`
	Folder    string              `json:"folder"`
	Parent    string              `json:"parent,omitempty"`
	Authority artifacts.Authority `json:"authority"`
}

// BrandFile is one file an adoption serves, as the record states it.
type BrandFile struct {
	// Slot is the file's place in the brand (brandformat.Slot).
	Slot        string `json:"slot"`
	ContentType string `json:"content_type"`

	// Object is a fresh object this adoption uploaded, also for each logo
	// and face copied from the kit (never the kit's own object, which its
	// retention may remove). Its SHA-256 hex is the path the artifact
	// listener serves it at.
	Object objstore.Object `json:"object"`
}

// BrandAdoptPayload adopts one brand kit version as the company's brand, on the
// brand subject. Its scope is the brand and the kit ([BrandTerm], the kit's
// artifact term), so it orders behind any record about the kit; the apply
// refuses `version_not_found` if the version is gone by then.
type BrandAdoptPayload struct {
	V int `json:"v"`

	// Kit and KitVersion are the brand artifact and the version adopted.
	Kit        string `json:"kit"`
	KitVersion int    `json:"kit_version"`

	// BrandJSON is the version's canonical brand.json, byte for byte.
	BrandJSON []byte `json:"brand_json"`

	// CSSSha is the generated stylesheet's SHA-256 in hex, which is also
	// the hash of the `css` file below.
	CSSSha string `json:"css_sha"`

	// Files are every file the adoption serves, the stylesheet first.
	Files []BrandFile `json:"files"`

	// Label is set on the adopted version, exempting it from the kit's
	// retention; empty when the version already carries one.
	Label string `json:"label,omitempty"`

	Authority artifacts.Authority `json:"authority"`
}

// BrandClearPayload withdraws the company's adopted brand. Pages pinned to a
// stylesheet keep it while a retained version names it.
type BrandClearPayload struct {
	V         int                 `json:"v"`
	Authority artifacts.Authority `json:"authority"`
}

// Eviction is a node's eviction from this log, or its readmission. A gate
// pinned at [GateRecordVersion] for ever: its shape only grows by addition.
type Eviction struct {
	V      int    `json:"v"`
	NodeID string `json:"node_id"`

	EvictedBy string    `json:"evicted_by,omitempty"`
	EvictedAt time.Time `json:"evicted_at"`

	// Readmitted makes this an inverse commit, not a delete, so a node
	// evicted, readmitted and evicted again reads correctly.
	Readmitted bool `json:"readmitted,omitempty"`
}

// Generation is a reanchor's record: this log was recreated, and every
// position below this generation is comparable and safely stale.
type Generation struct {
	V           int       `json:"v"`
	Generation  uint32    `json:"generation"`
	By          string    `json:"by,omitempty"`
	PrevHighest uint64    `json:"prev_highest,omitempty"`
	StreamAt    time.Time `json:"stream_created_at"`
}

// DecodeMutation reads the typed payload for one record.
//
// It dispatches on (kind, op) alone: an unknown pair is an error naming both,
// never a nil payload the applier would read as an empty change.
func DecodeMutation(rec MutationRecord) (any, error) {
	k, op := rec.Subject.Kind, rec.Op
	switch {
	case (k == KindArtifact || k == KindSource) && op == OpCreate:
		return decodePayload[CreatePayload](rec)
	case k == KindArtifact && (op == OpPublish || op == OpRestoreVersion):
		return decodePayload[PublishPayload](rec)
	case k == KindArtifact && op == OpPatch:
		return decodePayload[PatchPayload](rec)
	case k == KindArtifact && op == OpShare:
		return decodePayload[SharePayload](rec)
	case k == KindArtifact && op == OpUnshare:
		return decodePayload[UnsharePayload](rec)
	case k == KindArtifact && op == OpTransfer:
		return decodePayload[TransferPayload](rec)
	case k == KindArtifact && op == OpLinkWorkItem:
		return decodePayload[LinkPayload](rec)
	case k == KindArtifact && op == OpMove:
		return decodePayload[MovePayload](rec)
	case k == KindArtifact && (op == OpTrash || op == OpRestore || op == OpPurgeBegin || op == OpPurge):
		return decodePayload[StatusPayload](rec)
	case k == KindAsset && op == OpCreate:
		return decodePayload[AssetPayload](rec)
	case k == KindAsset && op == OpRemove:
		return decodePayload[StatusPayload](rec)
	case k == KindReader && (op == OpPin || op == OpUnpin || op == OpOpened ||
		op == OpReadElevated):
		return decodePayload[ReaderPayload](rec)
	case k == KindReader && op == OpInboxMarks:
		return decodePayload[PersonMarks](rec)
	case k == KindEviction && op == OpEviction:
		return decodePayload[Eviction](rec)
	case k == KindGeneration && op == OpGeneration:
		return decodePayload[Generation](rec)
	case k == KindFolder && op == OpCreate:
		return decodePayload[FolderCreatePayload](rec)
	case k == KindFolder && op == OpRename:
		return decodePayload[FolderRenamePayload](rec)
	case k == KindFolder && op == OpMove:
		return decodePayload[FolderMovePayload](rec)
	case k == KindFolder && op == OpRemove:
		return decodePayload[FolderRemovePayload](rec)
	case k == KindBrand && op == OpAdopt:
		return decodePayload[BrandAdoptPayload](rec)
	case k == KindBrand && op == OpClear:
		return decodePayload[BrandClearPayload](rec)
	case k == KindComment && op == OpCommentOpen:
		return decodePayload[CommentOpenPayload](rec)
	case k == KindComment && op == OpCommentReply:
		return decodePayload[CommentReplyPayload](rec)
	case k == KindComment && op == OpCommentEdit:
		return decodePayload[CommentEditPayload](rec)
	case k == KindComment && op == OpCommentRemove:
		return decodePayload[CommentRemovePayload](rec)
	case k == KindComment && (op == OpCommentResolve || op == OpCommentReopen):
		return decodePayload[CommentStatusPayload](rec)
	case k == KindBarrier && op == OpBarrier, k == KindTrimmed && op == OpTrimmed:
		// The two payload-free records: nil is their value, and the
		// applier's case for each writes nothing of this domain's.
		return nil, nil
	}
	return nil, fmt.Errorf("artifactsdomain: no payload shape for (%s, %s): a "+
		"record's kind and op together name its payload, and a pair this build "+
		"does not know is a newer peer's record rather than an empty one", k, op)
}

// decodePayload reads one typed payload.
func decodePayload[T any](rec MutationRecord) (T, error) {
	var out T
	if len(rec.Mutation) == 0 {
		return out, fmt.Errorf("artifactsdomain: the record on %s carries no "+
			"payload, and (%s, %s) needs one", rec.Subject, rec.Subject.Kind, rec.Op)
	}
	if err := json.Unmarshal(rec.Mutation, &out); err != nil {
		return out, fmt.Errorf("artifactsdomain: decode the %s payload on %s: %w",
			rec.Op, rec.Subject, err)
	}
	return out, nil
}

// EncodeBarrier renders the read index's payload-free append: the framework
// owns when, the domain what a record on its log looks like.
//
// An op id is refused: it becomes the message id, and a repeat inside the
// duplicate window is answered from the dedupe cache with no quorum round
// trip, the one claim a barrier must never fake.
func EncodeBarrier(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.BarrierKind {
		return nil, fmt.Errorf("artifactsdomain: %q is not a barrier envelope", env.Kind)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("artifactsdomain: a barrier carries no op id and " +
			"this one has one: an op id becomes a message id, and a duplicate " +
			"acknowledgement is served with no quorum round trip at all")
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			Subject: BarrierSubject(), Op: OpBarrier,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen,
		},
	})
}

// EncodeTrimmed renders the trimmed record ([statelog.TrimmedEnvelope],
// ADR-0080) for this domain's applier, split as [EncodeBarrier] is: the
// framework also owns what it removes and the floor its subject names.
//
// The writer travels, unlike a barrier's: the record removes rows, so the
// eviction gate must drop an evicted node's. No op id: it would become a
// message id and a ledger row nothing resolves. The floor is checked here, not
// in [Subject.Validate], which the read path runs too: a refusal there would
// stop the applier on every node, where the framework's apply faults instead.
func EncodeTrimmed(env statelog.Envelope) ([]byte, error) {
	if env.Kind != statelog.TrimmedKind || env.Subject.Kind != statelog.TrimmedKind {
		return nil, fmt.Errorf("artifactsdomain: a %q envelope on %q is not a "+
			"trimmed record", env.Kind, env.Subject)
	}
	if env.OpID != "" {
		return nil, fmt.Errorf("artifactsdomain: a trimmed record carries no op " +
			"id and this one has one: an op id becomes a message id and a ledger " +
			"row nothing resolves")
	}
	floor, err := statelog.TrimmedFloor(Domain{}.Stream().Name, env.Subject)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: %w", err)
	}
	return Encode(MutationRecord{
		RecordEnvelope: RecordEnvelope{
			// PINNED, for the barrier's reason: see [baseRecordVersion].
			V:       baseRecordVersion,
			Subject: TrimmedSubject(floor), Op: OpTrimmed,
			Scope: ScopeSet{Subject: true}, Gen: env.Gen, Writer: env.Writer,
		},
	})
}
