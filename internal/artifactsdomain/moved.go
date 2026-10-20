package artifactsdomain

import (
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT A COMMITTED BATCH MOVED, said after the commit.
//
// The serving origin keeps an authorization verdict per open document and the
// socket hub keeps rooms per artifact, and both must hear the moment a share is
// withdrawn, an artifact is trashed or a version lands. The apply is the only
// thing that sees every record on EVERY data node (the change feed relays a
// record to one), so each committed batch hands the engine what it moved, built
// from the rows the apply wrote rather than re-read, and the engine fans it out
// ([Engine.OnArtifactsMoved] in internal/engine).
//
// OVER-REPORTING IS THE SAFE DIRECTION: a body the store re-runs fills the list
// twice, and a listener told twice re-reads once more and nothing else. Under-
// reporting is a revoked share an open page keeps serving.

// MoveKind is what one movement was.
type MoveKind string

// The movements an applier can tell.
const (
	// MoveVersion is a new head version: a create, a publish or a rollback.
	MoveVersion MoveKind = "version"

	// MoveMeta is a change to the head's displayed fields or a label.
	MoveMeta MoveKind = "meta"

	// MoveShare is a share granted, changed or withdrawn.
	MoveShare MoveKind = "share"

	// MoveAsset is an asset added or removed.
	MoveAsset MoveKind = "asset"

	// MoveTrash and MoveRestore are the trash stamp set and cleared.
	MoveTrash   MoveKind = "trash"
	MoveRestore MoveKind = "restore"

	// MovePurge is the artifact gone for good.
	MovePurge MoveKind = "purge"

	// MoveTransfer is ownership moved.
	MoveTransfer MoveKind = "transfer"

	// MoveFolder is the artifact filed in another folder. Nothing about who
	// may open it changed (a folder grants nothing), so it narrows nothing;
	// it is told so an open page names the folder it is in now. A folder's
	// removal, which re-files everything it held, tells nothing per
	// artifact: an unbounded folder would be as many movements in one
	// batch, for a label every page reads again on its own.
	MoveFolder MoveKind = "folder"

	// MoveReset names NO artifact: every artifact on this node may have
	// moved with no committed record to say which, because a batch retained
	// a record this build cannot read, or an adoption replaced the whole
	// estate. A listener drops everything it decided from these rows.
	MoveReset MoveKind = "reset"
)

// MoveKinds are every kind of movement.
var MoveKinds = []MoveKind{
	MoveVersion, MoveMeta, MoveShare, MoveAsset, MoveTrash, MoveRestore, MovePurge,
	MoveTransfer, MoveFolder, MoveReset,
}

// Moved is one artifact's movement in a committed batch.
//
// TAGGED FOR THE WIRE, because a node whose copy is out of service hears a
// sound peer's movements relayed (internal/engine's live relay), and a
// relayed movement is this encoding.
type Moved struct {
	// Artifact is the artifact that moved; zero for [MoveReset].
	Artifact artifacts.ID `json:"artifact,omitzero"`

	What MoveKind `json:"what"`

	// AccessEpoch is the artifact's access epoch after the record: what a
	// view ticket minted before a narrowing is now refused by.
	AccessEpoch int64 `json:"access_epoch,omitempty"`

	// Head is the artifact's head version after the record.
	Head int `json:"head,omitempty"`

	// Position is the record's position on the log.
	Position statelog.Position `json:"position,omitzero"`
}

// Reset is the movement that names everything.
func Reset() Moved { return Moved{What: MoveReset} }
