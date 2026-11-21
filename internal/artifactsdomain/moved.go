package artifactsdomain

import (
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHAT A COMMITTED BATCH MOVED (package doc, "What a batch moved"). The apply,
// not the change feed, reports it: only the apply sees every record on EVERY
// data node; the feed relays a record to one.
//
// OVER-REPORTING IS THE SAFE DIRECTION: a body the store re-runs fills the list
// twice, and a listener told twice re-reads once more. Under-reporting is a
// revoked share an open page keeps serving.

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

	// MoveFolder is the artifact filed in another folder: it narrows nothing
	// (a folder grants nothing), and is told so an open page names its folder.
	// A folder's removal tells nothing per artifact: an unbounded folder would
	// be as many movements in one batch, for a label pages re-read anyway.
	MoveFolder MoveKind = "folder"

	// MoveComment is a comment thread opened, a remark added, edited or
	// removed, or a thread resolved or reopened (ADR-0154). It narrows
	// nothing: an open page re-reads its threads.
	MoveComment MoveKind = "comment"

	// MoveReset names NO artifact: any artifact on this node may have moved
	// (a batch retained a record this build cannot read, or an adoption
	// replaced the estate). A listener drops everything it decided from these
	// rows.
	MoveReset MoveKind = "reset"
)

// MoveKinds are every kind of movement.
var MoveKinds = []MoveKind{
	MoveVersion, MoveMeta, MoveShare, MoveAsset, MoveTrash, MoveRestore, MovePurge,
	MoveTransfer, MoveFolder, MoveReset, MoveComment,
}

// Moved is one artifact's movement in a committed batch. It is TAGGED FOR THE
// WIRE: a node whose copy is out of service hears a sound peer's movements
// relayed in this encoding (internal/engine's live relay).
type Moved struct {
	// Artifact is the artifact that moved; zero for [MoveReset].
	Artifact artifacts.ID `json:"artifact,omitzero"`

	What MoveKind `json:"what"`

	// AccessEpoch is the artifact's access epoch after the record: what a
	// view ticket minted before a narrowing is now refused by.
	AccessEpoch int64 `json:"access_epoch,omitempty"`

	// Head is the artifact's head version after the record.
	Head int `json:"head,omitempty"`

	// Position is the record's position on the log, and At the broker's
	// instant for it, which every node reads alike and the socket's push is
	// timed from (internal/objectives' change visibility).
	Position statelog.Position `json:"position,omitzero"`
	At       time.Time         `json:"at,omitzero"`
}

// Reset is the movement that names everything.
func Reset() Moved { return Moved{What: MoveReset} }
