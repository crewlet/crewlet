package artifactsdomain

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FOLDER TREE (ADR-0047: an artifact lives in one folder, and a folder
// grants nothing).
//
// One tree per company, on ONE subject ([FolderSubject]). Each artifact is
// filed in at most one folder ([Head.Folder], '' for the top level), and
// filing it never widens or narrows who may open it: an artifact's access is
// still its owner and its own shares (ADR-0034). A folder's OWN visibility is
// its top-level folder's owner and shares, decided as a relation exactly as an
// artifact's is (authz.KindArtifactFolder), so every ancestor of a folder a
// reader sees is one they see too, and sibling names never disclose a folder
// the reader cannot see.
//
// Filing asks two decisions, both the API's: editor on the artifact (with the
// artifacts app) and contributor on the destination folder, each through the
// requester ceiling. The folder's owner, the owner's lead and an administrator
// may rename, move, remove and unfile anything they can see. Removing a folder
// moves everything in it up one level; nothing is trashed and nobody loses
// access.

// FolderNamespace scopes the derivation of a folder's id from its creating
// operation and its place in the chain. FIXED for the life of the format: a new
// namespace would give a retry that straddles an upgrade a second folder.
var FolderNamespace = uuid.MustParse("3b8e2f61-5c4a-5d09-8e7f-1a2b3c4d5e6f")

// FolderIDOf is the folder a create under opID makes at index in its chain.
func FolderIDOf(opID string, index int) uuid.UUID {
	return uuid.NewSHA1(FolderNamespace, []byte(opID+"/"+strconv.Itoa(index)))
}

// FolderVisibility is a top-level folder's preset, the two the product offers:
// everyone at the company (a company share as contributor) or only its owner
// (no share).
type FolderVisibility string

const (
	// FolderCompany is "Everyone at <company>": the company holds a
	// contributor share on the top-level folder.
	FolderCompany FolderVisibility = "company"

	// FolderPrivate is "Only you": no share, so its owner (and the owner's
	// lead, for an agent's) alone sees it.
	FolderPrivate FolderVisibility = "private"

	// FolderCustom is a top-level folder whose shares are neither preset:
	// granted through the share machinery a later surface offers. Answered,
	// never asked for.
	FolderCustom FolderVisibility = "custom"
)

// FolderVisibilities are the presets a create may ask for, in the order the
// product offers them.
var FolderVisibilities = []FolderVisibility{FolderCompany, FolderPrivate}

// Valid reports whether v is a preset a create may ask for.
func (v FolderVisibility) Valid() bool { return v == FolderCompany || v == FolderPrivate }

// Folder is one folder as the tree and a point read show it.
type Folder struct {
	ID string `json:"id"`

	// Parent is the folder it sits in, empty for a top-level folder, and
	// Root its top-level folder (its own id for a top-level folder).
	Parent string `json:"parent,omitempty"`
	Root   string `json:"root"`

	Name string `json:"name"`

	Owner     string        `json:"owner"`
	OwnerKind iam.ActorKind `json:"owner_kind"`

	// Depth is 1 for a top-level folder; MaxDepth the bound stamped when it
	// was created.
	Depth    int `json:"depth"`
	MaxDepth int `json:"max_depth"`

	// Visibility is the top-level folder's preset, read off its shares.
	Visibility FolderVisibility `json:"visibility"`

	// Shares are the top-level folder's shares: what a decision about the
	// folder is made on. A surface shows them to the folder's owner and
	// managers only, as an artifact's are.
	Shares []artifacts.Share `json:"shares,omitempty"`

	Status    FolderStatus `json:"status"`
	CreatedBy iam.Actor    `json:"created_by"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedBy iam.Actor    `json:"updated_by"`
	UpdatedAt time.Time    `json:"updated_at"`

	// Version is the packed position of the last record that wrote the
	// folder's row.
	Version int64 `json:"version"`
}

// FolderStatus is whether a folder is live or a removed tombstone.
type FolderStatus string

const (
	FolderLive    FolderStatus = "live"
	FolderRemoved FolderStatus = "removed"
)

// FolderItem is one folder of a viewer's tree, with what is directly in it
// that the viewer can see.
type FolderItem struct {
	Folder

	// Artifacts and Folders count what sits DIRECTLY in the folder that the
	// viewer can see, from the listing's own visibility predicate in the
	// same transaction: a total over the subtree is the client's sum.
	Artifacts int `json:"artifacts"`
	Folders   int `json:"folders"`
}

// FolderTree is every live folder a viewer can see, parents before children
// and siblings A to Z, and what it is true as of.
type FolderTree struct {
	Folders []FolderItem `json:"folders"`

	// TopLevel is how many artifacts the viewer can see that are filed in
	// no folder they can see ("Not in a folder").
	TopLevel int `json:"top_level"`

	Level    statelog.ReadLevel `json:"read_level"`
	Position statelog.Position  `json:"position"`
	Complete bool               `json:"complete"`
}

// FolderCreateInput is one folder, or a chain of them, to create.
type FolderCreateInput struct {
	// Parent is the folder the chain is created in, empty for the top
	// level.
	Parent string

	// Names is the chain beneath Parent, parents first: one name, or a
	// path's names (at most artifacts.MaxFolderDepth), each checked by
	// foldername.Check.
	Names []string

	// Parents makes the create a `mkdir -p`: a name that already exists in
	// its place is reused rather than refused, and only the missing tail is
	// created, all in one record. Without it Names is exactly one name, and
	// an existing sibling of that name is ErrFolderNameTaken.
	Parents bool

	// Visibility is a top-level folder's preset, required when Parent is
	// empty and refused otherwise: a subfolder is decided by its top-level
	// folder.
	Visibility FolderVisibility

	// Owner is the seat handle (or machine login) that owns the folders,
	// and OwnerKind its actor kind.
	Owner     string
	OwnerKind iam.ActorKind
}

// FolderCreated is the chain a create answers, parents first: the folders it
// made and the ones a `mkdir -p` found already there.
type FolderCreated struct {
	Folders []Folder `json:"folders"`

	// Created is how many of Folders this write made.
	Created int `json:"created"`

	Result statelog.Result `json:"result"`
}

// ---- the writer's folder operations --------------------------------------- //

// CreateFolders creates one folder, or a chain of them, on the tree subject.
func (w *Writer) CreateFolders(_ context.Context, _ Call, _ FolderCreateInput) (FolderCreated, error) {
	return FolderCreated{}, fmt.Errorf("%w: create a folder", ErrNotBuilt)
}

// RenameFolder renames one folder.
func (w *Writer) RenameFolder(_ context.Context, _ Call, _, _ string) (Written, error) {
	return Written{}, fmt.Errorf("%w: rename a folder", ErrNotBuilt)
}

// MoveFolder moves one folder, and everything beneath it, under parent (empty
// for the top level).
func (w *Writer) MoveFolder(_ context.Context, _ Call, _, _ string) (Written, error) {
	return Written{}, fmt.Errorf("%w: move a folder", ErrNotBuilt)
}

// RemoveFolder removes one folder, moving its child folders and every artifact
// in it up to its parent.
func (w *Writer) RemoveFolder(_ context.Context, _ Call, _ string) (Written, error) {
	return Written{}, fmt.Errorf("%w: remove a folder", ErrNotBuilt)
}

// Move files one artifact in folder (empty for the top level), on the
// artifact's own subject.
func (w *Writer) Move(_ context.Context, _ Call, _ artifacts.ID, _ string) (Written, error) {
	return Written{}, fmt.Errorf("%w: move an artifact", ErrNotBuilt)
}

// ---- the reader's folder questions ---------------------------------------- //

// Folders answers every live folder v can see, with what is directly in each.
func (r *Reader) Folders(_ context.Context, _ Viewer, _ statelog.Freshness) (FolderTree, error) {
	return FolderTree{}, fmt.Errorf("%w: read the folder tree", ErrNotBuilt)
}

// Folder answers one folder by id, live or removed, with its top-level
// folder's shares, whatever the caller may see: what a decision about the
// folder is made on. The surface decides before it answers anything, and a
// caller the decision refuses is told ErrFolderNotFound.
func (r *Reader) Folder(_ context.Context, _ string, _ statelog.Freshness) (Folder, statelog.Position, error) {
	return Folder{}, statelog.Position{}, fmt.Errorf("%w: read a folder", ErrNotBuilt)
}

// ResolveFolder resolves a folder reference (an id, or a path of names
// separated by "/") among the live folders EVERY viewer in within can see: an
// agent and the requester it acts for. A path naming none is ErrFolderNotFound;
// one naming several is a *FolderAmbiguous.
func (r *Reader) ResolveFolder(_ context.Context, _ string, _ []Viewer, _ statelog.Freshness) (Folder, error) {
	return Folder{}, fmt.Errorf("%w: resolve a folder", ErrNotBuilt)
}

// ---- the applier's folder records ---------------------------------------- //

// applyFolder applies one record on the tree subject.
func (a *Applier) applyFolder(_ context.Context, _ *sql.Tx, c applyContext) (int, error) {
	return 0, fmt.Errorf("%w: apply the folder record at %s", ErrNotBuilt, c.position)
}

// applyMove files one artifact in another folder.
func (a *Applier) applyMove(_ context.Context, _ *sql.Tx, c applyContext, _ string, _ MovePayload) (int, error) {
	return 0, fmt.Errorf("%w: apply the move at %s", ErrNotBuilt, c.position)
}
