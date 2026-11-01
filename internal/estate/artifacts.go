package estate

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ARTIFACTS ESTATE AS OPERATIONS: every read and write a tool, the serving
// origin or the API makes of an artifact, answered by a data node whose copy
// serves (this one where it does), so a node holding no data acts through one
// that does (ADR-0025).
//
// READS ARE FLOORED ON THE ARTIFACTS LOG, the log every answer here is read
// from, so a read after a write is served by a node that has applied it. WRITES
// carry the caller's operation key on their [artifactsdomain.Call]: keyed, a
// write derives every record id from it and a repeat is the same operation, so
// an unanswered one is asked of the next data node under the same key; unkeyed
// it is never repeated ([ErrOutcomeUnknown]). The authority a write is decided
// under crosses whole, decided on the asking node where the grants are.

// ArtifactsReader is the artifacts read side the surfaces ask of: exactly the
// domain reader's method set, so a consumer cannot tell local from routed.
type ArtifactsReader interface {
	Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (
		artifactsdomain.Head, statelog.Position, error)
	List(ctx context.Context, v artifactsdomain.Viewer, q artifactsdomain.ListQuery) (
		artifactsdomain.ListPage, error)
	Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error)
	Versions(ctx context.Context, id artifacts.ID, cursor string, limit int) (
		artifactsdomain.VersionPage, error)
	Assets(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error)
	History(ctx context.Context, id artifacts.ID, cursor string, limit int) (
		artifactsdomain.HistoryPage, error)
	HeldObjects(ctx context.Context, id artifacts.ID, hashes []objstore.Hash) (
		map[objstore.Hash]objstore.Object, error)
	SeenBy(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Read, error)
	Usage(ctx context.Context) (artifactsdomain.Usage, error)
	Trail(ctx context.Context, q artifactsdomain.TrailQuery) (artifactsdomain.TrailPage, error)
	TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]artifacts.ID, error)

	// The folder tree.
	Folders(ctx context.Context, v artifactsdomain.Viewer, fresh statelog.Freshness) (
		artifactsdomain.FolderTree, error)
	Folder(ctx context.Context, id string, fresh statelog.Freshness) (
		artifactsdomain.Folder, statelog.Position, error)
	ResolveFolder(ctx context.Context, ref string, within []artifactsdomain.Viewer,
		fresh statelog.Freshness) (artifactsdomain.Folder, error)

	// The adopted brand.
	Brand(ctx context.Context, fresh statelog.Freshness) (artifactsdomain.Adoption, bool, error)
	BrandFile(ctx context.Context, hash objstore.Hash) (artifactsdomain.BrandFile, error)
}

// ArtifactsWriter is the artifacts write side: exactly the domain writer's
// method set.
type ArtifactsWriter interface {
	Create(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.CreateInput) (
		artifactsdomain.Published, error)
	Publish(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.PublishInput) (
		artifactsdomain.Published, error)
	Patch(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, p artifactsdomain.Patch) (
		artifactsdomain.Written, error)
	Share(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, s artifacts.Share) (
		artifactsdomain.Written, error)
	Unshare(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, a artifacts.Audience) (
		artifactsdomain.Written, error)
	Transfer(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, toSeat string) (
		artifactsdomain.Written, error)
	LinkWorkItem(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, item string) (
		artifactsdomain.Written, error)
	Trash(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	Restore(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	Purge(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	AddAsset(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.AssetInput) (
		artifactsdomain.Asset, artifactsdomain.Written, error)
	RemoveAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, asset uuid.UUID) (
		artifactsdomain.Written, error)
	Pin(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, pinned bool) (
		artifactsdomain.Written, error)
	Opened(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (artifactsdomain.Written, error)
	ReadElevated(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, version int) (
		artifactsdomain.Written, error)

	// The folder tree.
	CreateFolders(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.FolderCreateInput) (
		artifactsdomain.FolderCreated, error)
	RenameFolder(ctx context.Context, c artifactsdomain.Call, id, name string) (artifactsdomain.Written, error)
	MoveFolder(ctx context.Context, c artifactsdomain.Call, id, parent string) (artifactsdomain.Written, error)
	RemoveFolder(ctx context.Context, c artifactsdomain.Call, id string) (artifactsdomain.Written, error)
	Move(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, folder string) (
		artifactsdomain.Written, error)

	// The adopted brand.
	AdoptBrand(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.BrandAdoptInput) (
		artifactsdomain.Written, error)
	ClearBrand(ctx context.Context, c artifactsdomain.Call) (artifactsdomain.Written, error)
}

// ArtifactsHalf is the artifacts estate a data node serves: its reader and its
// writer, which every data node runs. A nil half is one this node's core runtime
// has not built, answered like any half a node does not run — and a nil writer
// beside a reader is a node that publishes nothing (a maintenance mode), whose
// reads are served and whose writes move on to a node that publishes.
type ArtifactsHalf struct {
	Reader ArtifactsReader
	Writer ArtifactsWriter
}

// artifactsReader is the half's reader, or [errNoHalf].
func (b Backend) artifactsReader() (ArtifactsReader, error) {
	if b.Artifacts.Reader == nil {
		return nil, errNoHalf
	}
	return b.Artifacts.Reader, nil
}

// artifactsWriter is the half's writer, or [errNoHalf].
func (b Backend) artifactsWriter() (ArtifactsWriter, error) {
	if b.Artifacts.Writer == nil {
		return nil, errNoHalf
	}
	return b.Artifacts.Writer, nil
}

// keyedCall reports an artifacts write the caller made REPEATABLE: one whose
// call carries the caller's own operation key, from which every record id is
// derived, so a repeat under it is the same operation.
func keyedCall(c artifactsdomain.Call) bool { return strings.TrimSpace(c.OpKey) != "" }

// ---- the reads ------------------------------------------------------------ //

type artifactArgs struct {
	ID    artifacts.ID
	Fresh statelog.Freshness
}

// anArtifact is [ArtifactsReader.Artifact]'s two answers, named.
type anArtifact struct {
	Head     artifactsdomain.Head
	Position statelog.Position
}

var opArtifact = define("artifacts.artifact", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactArgs) (anArtifact, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return anArtifact{}, err
		}
		head, at, err := r.Artifact(ctx, a.ID, a.Fresh)
		return anArtifact{Head: head, Position: at}, err
	})

type artifactListArgs struct {
	Viewer artifactsdomain.Viewer
	Query  artifactsdomain.ListQuery
}

var opArtifactList = define("artifacts.list", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactListArgs) (artifactsdomain.ListPage, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.ListPage{}, err
		}
		return r.List(ctx, a.Viewer, a.Query)
	})

type artifactVersionArgs struct {
	ID     artifacts.ID
	Number int
}

var opArtifactVersion = define("artifacts.version", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactVersionArgs) (artifactsdomain.Version, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.Version{}, err
		}
		return r.Version(ctx, a.ID, a.Number)
	})

// artifactPageArgs is one page of an artifact's versions or history.
type artifactPageArgs struct {
	ID     artifacts.ID
	Cursor string
	Limit  int
}

var opArtifactVersions = define("artifacts.versions", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactPageArgs) (artifactsdomain.VersionPage, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.VersionPage{}, err
		}
		return r.Versions(ctx, a.ID, a.Cursor, a.Limit)
	})

type artifactRefArgs struct {
	ID artifacts.ID
}

var opArtifactAssets = define("artifacts.assets", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactRefArgs) ([]artifactsdomain.Asset, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return nil, err
		}
		return r.Assets(ctx, a.ID)
	})

var opArtifactHistory = define("artifacts.history", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactPageArgs) (artifactsdomain.HistoryPage, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.HistoryPage{}, err
		}
		return r.History(ctx, a.ID, a.Cursor, a.Limit)
	})

type heldObjectsArgs struct {
	ID     artifacts.ID
	Hashes []objstore.Hash
}

var opArtifactHeldObjects = define("artifacts.held_objects", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a heldObjectsArgs) (map[objstore.Hash]objstore.Object, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return nil, err
		}
		return r.HeldObjects(ctx, a.ID, a.Hashes)
	})

var opArtifactSeenBy = define("artifacts.seen_by", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactRefArgs) ([]artifactsdomain.Read, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return nil, err
		}
		return r.SeenBy(ctx, a.ID)
	})

// artifactUsageArgs carries nothing: the counters are the company's.
type artifactUsageArgs struct{}

var opArtifactUsage = define("artifacts.usage", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ artifactUsageArgs) (artifactsdomain.Usage, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.Usage{}, err
		}
		return r.Usage(ctx)
	})

var opArtifactTrail = define("artifacts.trail", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q artifactsdomain.TrailQuery) (artifactsdomain.TrailPage, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.TrailPage{}, err
		}
		return r.Trail(ctx, q)
	})

type trashedBeforeArgs struct {
	Cutoff time.Time
	Limit  int
}

var opArtifactTrashedBefore = define("artifacts.trashed_before", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a trashedBeforeArgs) ([]artifacts.ID, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return nil, err
		}
		return r.TrashedBefore(ctx, a.Cutoff, a.Limit)
	})

// ---- the folder tree and the brand: reads ----------------------------------- //

type foldersArgs struct {
	Viewer artifactsdomain.Viewer
	Fresh  statelog.Freshness
}

var opArtifactFolders = define("artifacts.folders", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a foldersArgs) (artifactsdomain.FolderTree, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.FolderTree{}, err
		}
		return r.Folders(ctx, a.Viewer, a.Fresh)
	})

type folderArgs struct {
	ID    string
	Fresh statelog.Freshness
}

// aFolder is [ArtifactsReader.Folder]'s two answers, named.
type aFolder struct {
	Folder   artifactsdomain.Folder
	Position statelog.Position
}

var opArtifactFolder = define("artifacts.folder", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a folderArgs) (aFolder, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return aFolder{}, err
		}
		f, at, err := r.Folder(ctx, a.ID, a.Fresh)
		return aFolder{Folder: f, Position: at}, err
	})

type resolveFolderArgs struct {
	Ref    string
	Within []artifactsdomain.Viewer
	Fresh  statelog.Freshness
}

var opArtifactResolveFolder = define("artifacts.resolve_folder", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a resolveFolderArgs) (artifactsdomain.Folder, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.Folder{}, err
		}
		return r.ResolveFolder(ctx, a.Ref, a.Within, a.Fresh)
	})

type brandArgs struct {
	Fresh statelog.Freshness
}

// anAdoption is [ArtifactsReader.Brand]'s two answers, named.
type anAdoption struct {
	Adoption artifactsdomain.Adoption
	Adopted  bool
}

var opArtifactBrand = define("artifacts.brand", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a brandArgs) (anAdoption, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return anAdoption{}, err
		}
		adoption, adopted, err := r.Brand(ctx, a.Fresh)
		return anAdoption{Adoption: adoption, Adopted: adopted}, err
	})

type brandFileArgs struct {
	Hash objstore.Hash
}

var opArtifactBrandFile = define("artifacts.brand_file", opRead, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a brandFileArgs) (artifactsdomain.BrandFile, error) {
		r, err := b.artifactsReader()
		if err != nil {
			return artifactsdomain.BrandFile{}, err
		}
		return r.BrandFile(ctx, a.Hash)
	})

// ---- the folder tree and the brand: writes ---------------------------------- //

type folderCreateArgs struct {
	Call  artifactsdomain.Call
	Input artifactsdomain.FolderCreateInput
}

var opArtifactCreateFolders = define("artifacts.create_folders", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a folderCreateArgs) (artifactsdomain.FolderCreated, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.FolderCreated{}, err
		}
		return w.CreateFolders(ctx, a.Call, a.Input)
	}).repeatableWhen(func(a folderCreateArgs) bool { return keyedCall(a.Call) })

// folderNameArgs is a folder write that names its folder and one value: a
// rename's name, a move's parent.
type folderNameArgs struct {
	Call  artifactsdomain.Call
	ID    string
	Value string
}

var opArtifactRenameFolder = define("artifacts.rename_folder", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a folderNameArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.RenameFolder(ctx, a.Call, a.ID, a.Value)
	}).repeatableWhen(func(a folderNameArgs) bool { return keyedCall(a.Call) })

var opArtifactMoveFolder = define("artifacts.move_folder", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a folderNameArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.MoveFolder(ctx, a.Call, a.ID, a.Value)
	}).repeatableWhen(func(a folderNameArgs) bool { return keyedCall(a.Call) })

type folderCallArgs struct {
	Call artifactsdomain.Call
	ID   string
}

var opArtifactRemoveFolder = define("artifacts.remove_folder", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a folderCallArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.RemoveFolder(ctx, a.Call, a.ID)
	}).repeatableWhen(func(a folderCallArgs) bool { return keyedCall(a.Call) })

type artifactMoveArgs struct {
	Call   artifactsdomain.Call
	ID     artifacts.ID
	Folder string
}

var opArtifactMove = define("artifacts.move", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactMoveArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Move(ctx, a.Call, a.ID, a.Folder)
	}).repeatableWhen(func(a artifactMoveArgs) bool { return keyedCall(a.Call) })

type brandAdoptArgs struct {
	Call  artifactsdomain.Call
	Input artifactsdomain.BrandAdoptInput
}

var opArtifactAdoptBrand = define("artifacts.adopt_brand", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a brandAdoptArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.AdoptBrand(ctx, a.Call, a.Input)
	}).repeatableWhen(func(a brandAdoptArgs) bool { return keyedCall(a.Call) })

type brandClearArgs struct {
	Call artifactsdomain.Call
}

var opArtifactClearBrand = define("artifacts.clear_brand", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a brandClearArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.ClearBrand(ctx, a.Call)
	}).repeatableWhen(func(a brandClearArgs) bool { return keyedCall(a.Call) })

// ---- the writes ----------------------------------------------------------- //

type artifactCreateArgs struct {
	Call  artifactsdomain.Call
	Input artifactsdomain.CreateInput
}

var opArtifactCreate = define("artifacts.create", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactCreateArgs) (artifactsdomain.Published, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Published{}, err
		}
		return w.Create(ctx, a.Call, a.Input)
	}).repeatableWhen(func(a artifactCreateArgs) bool { return keyedCall(a.Call) })

type artifactPublishArgs struct {
	Call  artifactsdomain.Call
	Input artifactsdomain.PublishInput
}

var opArtifactPublish = define("artifacts.publish", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactPublishArgs) (artifactsdomain.Published, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Published{}, err
		}
		return w.Publish(ctx, a.Call, a.Input)
	}).repeatableWhen(func(a artifactPublishArgs) bool { return keyedCall(a.Call) })

type artifactPatchArgs struct {
	Call  artifactsdomain.Call
	ID    artifacts.ID
	Patch artifactsdomain.Patch
}

var opArtifactPatch = define("artifacts.patch", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactPatchArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Patch(ctx, a.Call, a.ID, a.Patch)
	}).repeatableWhen(func(a artifactPatchArgs) bool { return keyedCall(a.Call) })

type artifactShareArgs struct {
	Call  artifactsdomain.Call
	ID    artifacts.ID
	Share artifacts.Share
}

var opArtifactShare = define("artifacts.share", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactShareArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Share(ctx, a.Call, a.ID, a.Share)
	}).repeatableWhen(func(a artifactShareArgs) bool { return keyedCall(a.Call) })

type artifactUnshareArgs struct {
	Call     artifactsdomain.Call
	ID       artifacts.ID
	Audience artifacts.Audience
}

var opArtifactUnshare = define("artifacts.unshare", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactUnshareArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Unshare(ctx, a.Call, a.ID, a.Audience)
	}).repeatableWhen(func(a artifactUnshareArgs) bool { return keyedCall(a.Call) })

type artifactTransferArgs struct {
	Call   artifactsdomain.Call
	ID     artifacts.ID
	ToSeat string
}

var opArtifactTransfer = define("artifacts.transfer", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactTransferArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Transfer(ctx, a.Call, a.ID, a.ToSeat)
	}).repeatableWhen(func(a artifactTransferArgs) bool { return keyedCall(a.Call) })

type artifactLinkArgs struct {
	Call artifactsdomain.Call
	ID   artifacts.ID
	Item string
}

var opArtifactLinkWorkItem = define("artifacts.link_work_item", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactLinkArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.LinkWorkItem(ctx, a.Call, a.ID, a.Item)
	}).repeatableWhen(func(a artifactLinkArgs) bool { return keyedCall(a.Call) })

// artifactCallArgs is a write that names its artifact and nothing else.
type artifactCallArgs struct {
	Call artifactsdomain.Call
	ID   artifacts.ID
}

var opArtifactTrash = define("artifacts.trash", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactCallArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Trash(ctx, a.Call, a.ID)
	}).repeatableWhen(func(a artifactCallArgs) bool { return keyedCall(a.Call) })

var opArtifactRestore = define("artifacts.restore", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactCallArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Restore(ctx, a.Call, a.ID)
	}).repeatableWhen(func(a artifactCallArgs) bool { return keyedCall(a.Call) })

// A PURGE runs its dependents on the serving node, through that node's own
// writer, step by step under the gesture's operation: a keyed repeat elsewhere
// collapses every step that landed.
var opArtifactPurge = define("artifacts.purge", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactCallArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Purge(ctx, a.Call, a.ID)
	}).repeatableWhen(func(a artifactCallArgs) bool { return keyedCall(a.Call) })

type artifactAddAssetArgs struct {
	Call  artifactsdomain.Call
	Input artifactsdomain.AssetInput
}

// anAsset is [ArtifactsWriter.AddAsset]'s two answers, named.
type anAsset struct {
	Asset   artifactsdomain.Asset
	Written artifactsdomain.Written
}

var opArtifactAddAsset = define("artifacts.add_asset", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactAddAssetArgs) (anAsset, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return anAsset{}, err
		}
		asset, written, err := w.AddAsset(ctx, a.Call, a.Input)
		return anAsset{Asset: asset, Written: written}, err
	}).repeatableWhen(func(a artifactAddAssetArgs) bool { return keyedCall(a.Call) })

type artifactRemoveAssetArgs struct {
	Call  artifactsdomain.Call
	ID    artifacts.ID
	Asset uuid.UUID
}

var opArtifactRemoveAsset = define("artifacts.remove_asset", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactRemoveAssetArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.RemoveAsset(ctx, a.Call, a.ID, a.Asset)
	}).repeatableWhen(func(a artifactRemoveAssetArgs) bool { return keyedCall(a.Call) })

type artifactPinArgs struct {
	Call   artifactsdomain.Call
	ID     artifacts.ID
	Pinned bool
}

var opArtifactPin = define("artifacts.pin", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactPinArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Pin(ctx, a.Call, a.ID, a.Pinned)
	}).repeatableWhen(func(a artifactPinArgs) bool { return keyedCall(a.Call) })

var opArtifactOpened = define("artifacts.opened", opOnceWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactCallArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.Opened(ctx, a.Call, a.ID)
	}).repeatableWhen(func(a artifactCallArgs) bool { return keyedCall(a.Call) })

type artifactReadElevatedArgs struct {
	Call    artifactsdomain.Call
	ID      artifacts.ID
	Version int
}

// AN ELEVATED READ IS ALWAYS REPEATABLE: its operation is the day's, derived
// from the day, the admin, the artifact and the version whatever the call
// carries, so a repeat on another node is the same operation.
var opArtifactReadElevated = define("artifacts.read_elevated", opIdempotentWrite, artifactsDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a artifactReadElevatedArgs) (artifactsdomain.Written, error) {
		w, err := b.artifactsWriter()
		if err != nil {
			return artifactsdomain.Written{}, err
		}
		return w.ReadElevated(ctx, a.Call, a.ID, a.Version)
	})

// ---- the asking half ---------------------------------------------------- //

// Artifacts is the artifacts estate, answered by a data node whose copy serves:
// the reader's and the writer's method sets, so every surface is handed this on
// every node and cannot tell which node answered.
type Artifacts struct{ r *Router }

// Artifacts answers the artifacts estate.
func (r *Router) Artifacts() Artifacts { return Artifacts{r: r} }

// Artifact answers one artifact's head, and the position it was read at.
func (a Artifacts) Artifact(ctx context.Context, id artifacts.ID, fresh statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	out, err := call(ctx, a.r, opArtifact, nil, artifactArgs{ID: id, Fresh: fresh})
	return out.Head, out.Position, err
}

// List answers a page of the artifacts a viewer can see.
func (a Artifacts) List(ctx context.Context, v artifactsdomain.Viewer, q artifactsdomain.ListQuery) (
	artifactsdomain.ListPage, error) {
	return call(ctx, a.r, opArtifactList, nil, artifactListArgs{Viewer: v, Query: q})
}

// Version answers one retained version.
func (a Artifacts) Version(ctx context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error) {
	return call(ctx, a.r, opArtifactVersion, nil, artifactVersionArgs{ID: id, Number: number})
}

// Versions answers a page of an artifact's versions.
func (a Artifacts) Versions(ctx context.Context, id artifacts.ID, cursor string, limit int) (
	artifactsdomain.VersionPage, error) {
	return call(ctx, a.r, opArtifactVersions, nil, artifactPageArgs{ID: id, Cursor: cursor, Limit: limit})
}

// Assets answers an artifact's live assets.
func (a Artifacts) Assets(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error) {
	return call(ctx, a.r, opArtifactAssets, nil, artifactRefArgs{ID: id})
}

// History answers a page of an artifact's history.
func (a Artifacts) History(ctx context.Context, id artifacts.ID, cursor string, limit int) (
	artifactsdomain.HistoryPage, error) {
	return call(ctx, a.r, opArtifactHistory, nil, artifactPageArgs{ID: id, Cursor: cursor, Limit: limit})
}

// HeldObjects answers which digests the artifact already names.
func (a Artifacts) HeldObjects(ctx context.Context, id artifacts.ID, hashes []objstore.Hash) (
	map[objstore.Hash]objstore.Object, error) {
	out, err := call(ctx, a.r, opArtifactHeldObjects, nil, heldObjectsArgs{ID: id, Hashes: hashes})
	if out == nil && err == nil {
		out = map[objstore.Hash]objstore.Object{}
	}
	return out, err
}

// SeenBy answers who has opened an artifact.
func (a Artifacts) SeenBy(ctx context.Context, id artifacts.ID) ([]artifactsdomain.Read, error) {
	return call(ctx, a.r, opArtifactSeenBy, nil, artifactRefArgs{ID: id})
}

// Usage answers the company's counters.
func (a Artifacts) Usage(ctx context.Context) (artifactsdomain.Usage, error) {
	return call(ctx, a.r, opArtifactUsage, nil, artifactUsageArgs{})
}

// Trail answers a page of the company trail: every artifact's history.
func (a Artifacts) Trail(ctx context.Context, q artifactsdomain.TrailQuery) (artifactsdomain.TrailPage, error) {
	return call(ctx, a.r, opArtifactTrail, nil, q)
}

// TrashedBefore answers the artifacts trashed before cutoff.
func (a Artifacts) TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]artifacts.ID, error) {
	return call(ctx, a.r, opArtifactTrashedBefore, nil, trashedBeforeArgs{Cutoff: cutoff, Limit: limit})
}

// Create makes an artifact. Never repeated when unanswered (see
// [ErrOutcomeUnknown]) unless its call carries the caller's operation key,
// under which a repeat is the same operation. So for every write below.
func (a Artifacts) Create(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.CreateInput) (
	artifactsdomain.Published, error) {
	out, err := call(ctx, a.r, opArtifactCreate, nil, artifactCreateArgs{Call: c, Input: in})
	a.r.Observe(out.Result.Position)
	return out, err
}

// Publish adds a version.
func (a Artifacts) Publish(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.PublishInput) (
	artifactsdomain.Published, error) {
	out, err := call(ctx, a.r, opArtifactPublish, nil, artifactPublishArgs{Call: c, Input: in})
	a.r.Observe(out.Result.Position)
	return out, err
}

// Patch changes the head's displayed fields or a label.
func (a Artifacts) Patch(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	p artifactsdomain.Patch) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactPatch, nil, artifactPatchArgs{Call: c, ID: id, Patch: p}))
}

// Share grants one audience a role.
func (a Artifacts) Share(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	s artifacts.Share) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactShare, nil, artifactShareArgs{Call: c, ID: id, Share: s}))
}

// Unshare withdraws one audience's share.
func (a Artifacts) Unshare(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	audience artifacts.Audience) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactUnshare, nil,
		artifactUnshareArgs{Call: c, ID: id, Audience: audience}))
}

// Transfer moves ownership to another seat.
func (a Artifacts) Transfer(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	toSeat string) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactTransfer, nil,
		artifactTransferArgs{Call: c, ID: id, ToSeat: toSeat}))
}

// LinkWorkItem links an artifact to a tracker work item.
func (a Artifacts) LinkWorkItem(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	item string) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactLinkWorkItem, nil,
		artifactLinkArgs{Call: c, ID: id, Item: item}))
}

// Trash stamps an artifact trashed.
func (a Artifacts) Trash(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactTrash, nil, artifactCallArgs{Call: c, ID: id}))
}

// Restore takes an artifact out of the trash.
func (a Artifacts) Restore(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactRestore, nil, artifactCallArgs{Call: c, ID: id}))
}

// Purge removes an artifact permanently, its dependents first.
func (a Artifacts) Purge(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactPurge, nil, artifactCallArgs{Call: c, ID: id}))
}

// AddAsset adds one asset.
func (a Artifacts) AddAsset(ctx context.Context, c artifactsdomain.Call, in artifactsdomain.AssetInput) (
	artifactsdomain.Asset, artifactsdomain.Written, error) {
	out, err := call(ctx, a.r, opArtifactAddAsset, nil, artifactAddAssetArgs{Call: c, Input: in})
	a.r.Observe(out.Written.Result.Position)
	return out.Asset, out.Written, err
}

// RemoveAsset removes one asset.
func (a Artifacts) RemoveAsset(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	asset uuid.UUID) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactRemoveAsset, nil,
		artifactRemoveAssetArgs{Call: c, ID: id, Asset: asset}))
}

// Pin keeps or drops the caller's pin.
func (a Artifacts) Pin(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, pinned bool) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactPin, nil, artifactPinArgs{Call: c, ID: id, Pinned: pinned}))
}

// Opened marks when the caller last opened an artifact.
func (a Artifacts) Opened(ctx context.Context, c artifactsdomain.Call, id artifacts.ID) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactOpened, nil, artifactCallArgs{Call: c, ID: id}))
}

// ReadElevated records an elevated read.
func (a Artifacts) ReadElevated(ctx context.Context, c artifactsdomain.Call, id artifacts.ID,
	version int) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactReadElevated, nil,
		artifactReadElevatedArgs{Call: c, ID: id, Version: version}))
}

// Folders answers every folder a viewer can see, with what is directly in each.
func (a Artifacts) Folders(ctx context.Context, v artifactsdomain.Viewer, fresh statelog.Freshness) (
	artifactsdomain.FolderTree, error) {
	return call(ctx, a.r, opArtifactFolders, nil, foldersArgs{Viewer: v, Fresh: fresh})
}

// Folder answers one folder, and the position it was read at.
func (a Artifacts) Folder(ctx context.Context, id string, fresh statelog.Freshness) (
	artifactsdomain.Folder, statelog.Position, error) {
	out, err := call(ctx, a.r, opArtifactFolder, nil, folderArgs{ID: id, Fresh: fresh})
	return out.Folder, out.Position, err
}

// ResolveFolder resolves a folder id or path among the folders every viewer
// in within can see.
func (a Artifacts) ResolveFolder(ctx context.Context, ref string, within []artifactsdomain.Viewer,
	fresh statelog.Freshness) (artifactsdomain.Folder, error) {
	return call(ctx, a.r, opArtifactResolveFolder, nil,
		resolveFolderArgs{Ref: ref, Within: within, Fresh: fresh})
}

// Brand answers the adopted brand, and false when none is adopted.
func (a Artifacts) Brand(ctx context.Context, fresh statelog.Freshness) (artifactsdomain.Adoption, bool, error) {
	out, err := call(ctx, a.r, opArtifactBrand, nil, brandArgs{Fresh: fresh})
	return out.Adoption, out.Adopted, err
}

// BrandFile answers one file an adoption serves, by its hash.
func (a Artifacts) BrandFile(ctx context.Context, hash objstore.Hash) (artifactsdomain.BrandFile, error) {
	return call(ctx, a.r, opArtifactBrandFile, nil, brandFileArgs{Hash: hash})
}

// CreateFolders creates one folder, or a chain of them.
func (a Artifacts) CreateFolders(ctx context.Context, c artifactsdomain.Call,
	in artifactsdomain.FolderCreateInput) (artifactsdomain.FolderCreated, error) {
	out, err := call(ctx, a.r, opArtifactCreateFolders, nil, folderCreateArgs{Call: c, Input: in})
	a.r.Observe(out.Result.Position)
	return out, err
}

// RenameFolder renames one folder.
func (a Artifacts) RenameFolder(ctx context.Context, c artifactsdomain.Call, id, name string) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactRenameFolder, nil, folderNameArgs{Call: c, ID: id, Value: name}))
}

// MoveFolder moves one folder under parent.
func (a Artifacts) MoveFolder(ctx context.Context, c artifactsdomain.Call, id, parent string) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactMoveFolder, nil, folderNameArgs{Call: c, ID: id, Value: parent}))
}

// RemoveFolder removes one folder, moving what it holds up a level.
func (a Artifacts) RemoveFolder(ctx context.Context, c artifactsdomain.Call, id string) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactRemoveFolder, nil, folderCallArgs{Call: c, ID: id}))
}

// Move files one artifact in a folder.
func (a Artifacts) Move(ctx context.Context, c artifactsdomain.Call, id artifacts.ID, folder string) (
	artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactMove, nil, artifactMoveArgs{Call: c, ID: id, Folder: folder}))
}

// AdoptBrand adopts one version of a kit as the company's brand.
func (a Artifacts) AdoptBrand(ctx context.Context, c artifactsdomain.Call,
	in artifactsdomain.BrandAdoptInput) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactAdoptBrand, nil, brandAdoptArgs{Call: c, Input: in}))
}

// ClearBrand withdraws the company's adopted brand.
func (a Artifacts) ClearBrand(ctx context.Context, c artifactsdomain.Call) (artifactsdomain.Written, error) {
	return a.written(call(ctx, a.r, opArtifactClearBrand, nil, brandClearArgs{Call: c}))
}

// written observes a write's position for the next read's floor and answers it.
func (a Artifacts) written(out artifactsdomain.Written, err error) (artifactsdomain.Written, error) {
	a.r.Observe(out.Result.Position)
	return out, err
}
