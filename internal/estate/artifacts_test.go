package estate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// vault is a data node's artifacts estate as the cases reach it: every read
// answers a value naming what it was asked, every write records its operation
// and the call it was made under, and one artifact answers every refusal the
// writer has.
type vault struct {
	mu    sync.Mutex
	calls []string
	keys  []string
}

// refused is the artifact whose every write answers a moved head.
var refused = artifacts.IDFor("the refused artifact")

func (v *vault) For(context.Context) (Backend, bool) {
	return Backend{Artifacts: ArtifactsHalf{Reader: v, Writer: v}}, true
}

func (v *vault) wrote(op string, c artifactsdomain.Call) artifactsdomain.Written {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, op+":"+c.Authority.Actor.Name)
	v.keys = append(v.keys, c.OpKey)
	return artifactsdomain.Written{Result: statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpKey}}
}

func (v *vault) Artifact(_ context.Context, id artifacts.ID, fresh statelog.Freshness) (
	artifactsdomain.Head, statelog.Position, error) {
	if id == refused {
		return artifactsdomain.Head{}, statelog.Position{}, fmt.Errorf("%w: %s", artifactsdomain.ErrPurged, id)
	}
	return artifactsdomain.Head{ID: id, Title: string(fresh.Level),
			Shares: []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"},
				Role: artifacts.RoleEditor, GrantedBy: iam.Actor{Name: "jane", Kind: iam.ActorHuman}}}},
		statelog.Position{Stream: artifactsdomain.Domain{}.Stream().Name, Generation: 1, Seq: 7}, nil
}

func (v *vault) List(_ context.Context, viewer artifactsdomain.Viewer, q artifactsdomain.ListQuery) (
	artifactsdomain.ListPage, error) {
	return artifactsdomain.ListPage{Total: len(viewer.Units), NextContinuationToken: q.Q + "|" + string(q.Scope)}, nil
}

func (v *vault) Version(_ context.Context, id artifacts.ID, number int) (artifactsdomain.Version, error) {
	return artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Artifact: id, Number: number}}, nil
}

func (v *vault) Versions(_ context.Context, _ artifacts.ID, cursor string, limit int) (
	artifactsdomain.VersionPage, error) {
	return artifactsdomain.VersionPage{After: fmt.Sprintf("%s+%d", cursor, limit)}, nil
}

func (v *vault) Assets(_ context.Context, id artifacts.ID) ([]artifactsdomain.Asset, error) {
	return []artifactsdomain.Asset{{Artifact: id, Name: "logo.png"}}, nil
}

func (v *vault) History(_ context.Context, _ artifacts.ID, cursor string, limit int) (
	artifactsdomain.HistoryPage, error) {
	return artifactsdomain.HistoryPage{After: fmt.Sprintf("%s+%d", cursor, limit)}, nil
}

func (v *vault) HeldObjects(_ context.Context, _ artifacts.ID, hashes []objstore.Hash) (
	map[objstore.Hash]objstore.Object, error) {
	out := map[objstore.Hash]objstore.Object{}
	for _, h := range hashes {
		out[h] = objstore.Object{Key: objstore.KeyAt(time.Unix(1_700_000_000, 0)), Hash: h, Size: 3}
	}
	return out, nil
}

func (v *vault) SeenBy(context.Context, artifacts.ID) ([]artifactsdomain.Read, error) {
	return []artifactsdomain.Read{{Seat: "bob", OpenedAt: time.Unix(1_700_000_000, 0).UTC()}}, nil
}

func (v *vault) Usage(context.Context) (artifactsdomain.Usage, error) {
	return artifactsdomain.Usage{Artifacts: 3, Bytes: 42, Complete: true}, nil
}

func (v *vault) Trail(_ context.Context, q artifactsdomain.TrailQuery) (artifactsdomain.TrailPage, error) {
	return artifactsdomain.TrailPage{After: string(q.Op) + "+" + q.Cursor, Complete: true}, nil
}

func (v *vault) TrashedBefore(_ context.Context, _ time.Time, limit int) ([]artifacts.ID, error) {
	return []artifacts.ID{artifacts.IDFor(fmt.Sprint(limit))}, nil
}

func (v *vault) Create(_ context.Context, c artifactsdomain.Call, in artifactsdomain.CreateInput) (
	artifactsdomain.Published, error) {
	w := v.wrote("create", c)
	return artifactsdomain.Published{Artifact: artifacts.IDFor(in.Title), Number: 1, Result: w.Result}, nil
}

func (v *vault) Publish(_ context.Context, c artifactsdomain.Call, in artifactsdomain.PublishInput) (
	artifactsdomain.Published, error) {
	if in.Artifact == refused {
		return artifactsdomain.Published{}, &artifactsdomain.VersionMoved{
			Head: artifactsdomain.Head{ID: refused, HeadNumber: 9},
			Live: artifactsdomain.Version{VersionSummary: artifactsdomain.VersionSummary{Number: 9}},
		}
	}
	w := v.wrote("publish", c)
	return artifactsdomain.Published{Artifact: in.Artifact, Number: in.Base + 1, Result: w.Result}, nil
}

func (v *vault) Patch(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, _ artifactsdomain.Patch) (
	artifactsdomain.Written, error) {
	return v.wrote("patch", c), nil
}

func (v *vault) Share(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, s artifacts.Share) (
	artifactsdomain.Written, error) {
	if s.Audience.Kind == artifacts.AudienceCompany {
		return artifactsdomain.Written{}, artifactsdomain.ErrAgentCompanyShare
	}
	return v.wrote("share", c), nil
}

func (v *vault) Unshare(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, _ artifacts.Audience) (
	artifactsdomain.Written, error) {
	return v.wrote("unshare", c), nil
}

func (v *vault) Transfer(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, to string) (
	artifactsdomain.Written, error) {
	return v.wrote("transfer:"+to, c), nil
}

func (v *vault) Trash(_ context.Context, c artifactsdomain.Call, _ artifacts.ID) (artifactsdomain.Written, error) {
	return v.wrote("trash", c), nil
}

func (v *vault) Restore(_ context.Context, c artifactsdomain.Call, _ artifacts.ID) (artifactsdomain.Written, error) {
	return v.wrote("restore", c), nil
}

func (v *vault) Purge(_ context.Context, c artifactsdomain.Call, _ artifacts.ID) (artifactsdomain.Written, error) {
	return v.wrote("purge", c), nil
}

func (v *vault) AddAsset(_ context.Context, c artifactsdomain.Call, in artifactsdomain.AssetInput) (
	artifactsdomain.Asset, artifactsdomain.Written, error) {
	w := v.wrote("add_asset", c)
	return artifactsdomain.Asset{Artifact: in.Artifact, Name: in.Name}, w, nil
}

func (v *vault) RemoveAsset(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, _ uuid.UUID) (
	artifactsdomain.Written, error) {
	return v.wrote("remove_asset", c), nil
}

func (v *vault) Pin(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, pinned bool) (
	artifactsdomain.Written, error) {
	return v.wrote(fmt.Sprintf("pin:%v", pinned), c), nil
}

func (v *vault) LinkWorkItem(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, item string) (
	artifactsdomain.Written, error) {
	return v.wrote("link_work_item:"+item, c), nil
}

func (v *vault) Opened(_ context.Context, c artifactsdomain.Call, _ artifacts.ID) (artifactsdomain.Written, error) {
	return v.wrote("opened", c), nil
}

func (v *vault) ReadElevated(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, version int) (
	artifactsdomain.Written, error) {
	return v.wrote(fmt.Sprintf("read_elevated:%d", version), c), nil
}

// EVERY ARTIFACTS OPERATION CROSSES WHOLE: a node holding no data reaches a
// data node's artifacts estate with every argument (the call's authority and
// key included), every answer (a head's shares, a read's position, the held
// objects by digest), and every refusal keeps its identity: a moved head's
// fields for the merge, an agent's company share by its sentinel.
//
// Mutation: drop an argument from an operation's args type, or a typed error
// from typedKinds, and a row goes red.
// ambiguousPath is the folder path every resolution of which answers two
// candidates.
const ambiguousPath = "Reports/"

func (v *vault) Folders(_ context.Context, viewer artifactsdomain.Viewer, fresh statelog.Freshness) (
	artifactsdomain.FolderTree, error) {
	return artifactsdomain.FolderTree{TopLevel: len(viewer.Units), Folders: []artifactsdomain.FolderItem{{
		Folder:    artifactsdomain.Folder{ID: "f-" + string(fresh.Level), Visibility: artifactsdomain.FolderCompany},
		Artifacts: 3,
	}}}, nil
}

func (v *vault) Folder(_ context.Context, id string, fresh statelog.Freshness) (
	artifactsdomain.Folder, statelog.Position, error) {
	return artifactsdomain.Folder{ID: id, Name: string(fresh.Level)},
		statelog.Position{Stream: artifactsdomain.Domain{}.Stream().Name, Generation: 1, Seq: 8}, nil
}

func (v *vault) ResolveFolder(_ context.Context, ref string, within []artifactsdomain.Viewer,
	_ statelog.Freshness) (artifactsdomain.Folder, error) {
	if ref == ambiguousPath {
		return artifactsdomain.Folder{}, &artifactsdomain.FolderAmbiguous{Path: ref,
			Candidates: []artifactsdomain.Folder{{ID: "f-1"}, {ID: "f-2"}}}
	}
	return artifactsdomain.Folder{ID: ref + "|" + within[len(within)-1].Record}, nil
}

func (v *vault) Brand(_ context.Context, fresh statelog.Freshness) (artifactsdomain.Adoption, bool, error) {
	return artifactsdomain.Adoption{KitVersion: 3, CSSSha: string(fresh.Level)}, true, nil
}

func (v *vault) BrandFile(_ context.Context, hash objstore.Hash) (artifactsdomain.BrandFile, error) {
	return artifactsdomain.BrandFile{Slot: "css", Object: objstore.Object{Hash: hash}}, nil
}

func (v *vault) CreateFolders(_ context.Context, c artifactsdomain.Call, in artifactsdomain.FolderCreateInput) (
	artifactsdomain.FolderCreated, error) {
	w := v.wrote("create_folders:"+strings.Join(in.Names, "/"), c)
	return artifactsdomain.FolderCreated{Created: len(in.Names), Result: w.Result}, nil
}

func (v *vault) RenameFolder(_ context.Context, c artifactsdomain.Call, id, name string) (
	artifactsdomain.Written, error) {
	return v.wrote("rename_folder:"+id+":"+name, c), nil
}

func (v *vault) MoveFolder(_ context.Context, c artifactsdomain.Call, id, parent string) (
	artifactsdomain.Written, error) {
	return v.wrote("move_folder:"+id+":"+parent, c), nil
}

func (v *vault) RemoveFolder(_ context.Context, c artifactsdomain.Call, id string) (
	artifactsdomain.Written, error) {
	return v.wrote("remove_folder:"+id, c), nil
}

func (v *vault) Move(_ context.Context, c artifactsdomain.Call, _ artifacts.ID, folder string) (
	artifactsdomain.Written, error) {
	return v.wrote("move:"+folder, c), nil
}

func (v *vault) AdoptBrand(_ context.Context, c artifactsdomain.Call, in artifactsdomain.BrandAdoptInput) (
	artifactsdomain.Written, error) {
	if in.Version == 98 {
		return artifactsdomain.Written{}, &artifactsdomain.BrandIncomplete{
			Missing: []string{"logos.primary", "colors.primary"}}
	}
	if in.Version == 99 {
		return artifactsdomain.Written{}, &artifactsdomain.BrandContrast{Pair: brandformat.ContrastResult{
			ContrastPair:    brandformat.ContrastPairs[0],
			ForegroundColor: "#999999", GroundColor: "#f6f7fb", Ratio: 2.66,
		}}
	}
	return v.wrote(fmt.Sprintf("adopt_brand:%d", in.Version), c), nil
}

func (v *vault) ClearBrand(_ context.Context, c artifactsdomain.Call) (artifactsdomain.Written, error) {
	return v.wrote("clear_brand", c), nil
}

func TestEveryArtifactsOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	v := &vault{}
	stop, err := Serve(t.Context(), f.start(t), ServerOptions{
		Self: "data-d", Local: v, Envelopes: fleetEnvelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-d"} })
	ctx := t.Context()
	a := f.client.Artifacts()
	id := artifacts.IDFor("crossing")
	jane := artifactsdomain.Call{
		Authority: artifacts.Authority{Actor: iam.Actor{Name: "jane", Kind: iam.ActorHuman},
			Role: artifacts.RoleOwner, Origin: artifacts.OriginDashboard},
		OpKey: statelog.NewOpID(time.Now(), ""),
	}

	head, at, err := a.Artifact(ctx, id, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil || head.ID != id || head.Title != string(statelog.ReadSession) ||
		len(head.Shares) != 1 || head.Shares[0].GrantedBy.Name != "jane" || at.Seq != 7 {
		t.Errorf("artifact = (%+v, %v, %v)", head, at, err)
	}
	if _, _, err := a.Artifact(ctx, refused, statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, artifactsdomain.ErrPurged) {
		t.Errorf("a purged artifact's refusal became %v", err)
	}
	if page, err := a.List(ctx, artifactsdomain.Viewer{Record: "jane", Units: []string{"ENG", "OPS"}},
		artifactsdomain.ListQuery{Q: "report", Scope: artifactsdomain.ScopeShared}); err != nil ||
		page.Total != 2 || page.NextContinuationToken != "report|shared" {
		t.Errorf("list = (%+v, %v)", page, err)
	}
	if ver, err := a.Version(ctx, id, 4); err != nil || ver.Number != 4 || ver.Artifact != id {
		t.Errorf("version = (%+v, %v)", ver, err)
	}
	if page, err := a.Versions(ctx, id, "c", 5); err != nil || page.After != "c+5" {
		t.Errorf("versions = (%+v, %v)", page, err)
	}
	if page, err := a.History(ctx, id, "h", 6); err != nil || page.After != "h+6" {
		t.Errorf("history = (%+v, %v)", page, err)
	}
	if assets, err := a.Assets(ctx, id); err != nil || len(assets) != 1 || assets[0].Name != "logo.png" {
		t.Errorf("assets = (%+v, %v)", assets, err)
	}
	digest := objstore.HashOf([]byte("abc"))
	if held, err := a.HeldObjects(ctx, id, []objstore.Hash{digest}); err != nil || held[digest].Size != 3 {
		t.Errorf("held objects = (%+v, %v)", held, err)
	}
	if seen, err := a.SeenBy(ctx, id); err != nil || len(seen) != 1 || seen[0].Seat != "bob" {
		t.Errorf("seen by = (%+v, %v)", seen, err)
	}
	if usage, err := a.Usage(ctx); err != nil || usage.Bytes != 42 || !usage.Complete {
		t.Errorf("usage = (%+v, %v)", usage, err)
	}
	if trail, err := a.Trail(ctx, artifactsdomain.TrailQuery{Op: artifactsdomain.OpReadElevated,
		Cursor: "c"}); err != nil || trail.After != "read_elevated+c" || !trail.Complete {
		t.Errorf("trail = (%+v, %v)", trail, err)
	}
	if trash, err := a.TrashedBefore(ctx, time.Now(), 11); err != nil || len(trash) != 1 ||
		trash[0] != artifacts.IDFor("11") {
		t.Errorf("trashed before = (%+v, %v)", trash, err)
	}

	if got, err := a.Create(ctx, jane, artifactsdomain.CreateInput{Title: "made"}); err != nil ||
		got.Artifact != artifacts.IDFor("made") {
		t.Errorf("create = (%+v, %v)", got, err)
	}
	if got, err := a.Publish(ctx, jane, artifactsdomain.PublishInput{Artifact: id, Base: 2}); err != nil ||
		got.Number != 3 {
		t.Errorf("publish = (%+v, %v)", got, err)
	}
	_, err = a.Publish(ctx, jane, artifactsdomain.PublishInput{Artifact: refused, Base: 2})
	var moved *artifactsdomain.VersionMoved
	if !errors.As(err, &moved) || moved.Head.HeadNumber != 9 || moved.Live.Number != 9 {
		t.Errorf("a moved head's refusal crossed as %v", err)
	}
	if _, err := a.Share(ctx, jane, id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceCompany}, Role: artifacts.RoleViewer,
	}); !errors.Is(err, artifactsdomain.ErrAgentCompanyShare) {
		t.Errorf("a company share's refusal crossed as %v", err)
	}

	stale := statelog.Freshness{Level: statelog.ReadSession}
	if tree, err := a.Folders(ctx, artifactsdomain.Viewer{Record: "jane", Units: []string{"ENG"}}, stale); err != nil ||
		tree.TopLevel != 1 || len(tree.Folders) != 1 || tree.Folders[0].ID != "f-session" ||
		tree.Folders[0].Artifacts != 3 || tree.Folders[0].Visibility != artifactsdomain.FolderCompany {
		t.Errorf("folders = (%+v, %v)", tree, err)
	}
	if f, at, err := a.Folder(ctx, "f-9", stale); err != nil || f.ID != "f-9" || f.Name != "session" || at.Seq != 8 {
		t.Errorf("folder = (%+v, %v, %v)", f, at, err)
	}
	if f, err := a.ResolveFolder(ctx, "Marketing/Q4/", []artifactsdomain.Viewer{{Record: "agent"},
		{Record: "jane"}}, stale); err != nil || f.ID != "Marketing/Q4/|jane" {
		t.Errorf("resolve folder = (%+v, %v)", f, err)
	}
	_, err = a.ResolveFolder(ctx, ambiguousPath, []artifactsdomain.Viewer{{Record: "jane"}}, stale)
	var ambiguous *artifactsdomain.FolderAmbiguous
	if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 || ambiguous.Candidates[1].ID != "f-2" {
		t.Errorf("an ambiguous path's refusal crossed as %v", err)
	}
	if b, adopted, err := a.Brand(ctx, stale); err != nil || !adopted || b.KitVersion != 3 || b.CSSSha != "session" {
		t.Errorf("brand = (%+v, %v, %v)", b, adopted, err)
	}
	if f, err := a.BrandFile(ctx, digest); err != nil || f.Object.Hash != digest || f.Slot != "css" {
		t.Errorf("brand file = (%+v, %v)", f, err)
	}
	if got, err := a.CreateFolders(ctx, jane, artifactsdomain.FolderCreateInput{
		Names: []string{"Reports", "Weekly"}, Parents: true}); err != nil || got.Created != 2 {
		t.Errorf("create folders = (%+v, %v)", got, err)
	}
	_, err = a.AdoptBrand(ctx, jane, artifactsdomain.BrandAdoptInput{Version: 99})
	var contrast *artifactsdomain.BrandContrast
	if !errors.Is(err, artifactsdomain.ErrBrandContrast) || !errors.As(err, &contrast) ||
		contrast.Pair.Ratio != 2.66 || contrast.Pair.ForegroundColor != "#999999" {
		t.Errorf("a contrast refusal crossed as %v, without the pair that refused it", err)
	}
	_, err = a.AdoptBrand(ctx, jane, artifactsdomain.BrandAdoptInput{Version: 98})
	var incomplete *artifactsdomain.BrandIncomplete
	if !errors.Is(err, artifactsdomain.ErrBrandIncomplete) || !errors.As(err, &incomplete) ||
		strings.Join(incomplete.Missing, ",") != "logos.primary,colors.primary" {
		t.Errorf("an incomplete brand's refusal crossed as %v, without the fields it leaves out", err)
	}
	for name, write := range map[string]func() error{
		"patch": func() error { _, err := a.Patch(ctx, jane, id, artifactsdomain.Patch{}); return err },
		"share": func() error {
			_, err := a.Share(ctx, jane, id, artifacts.Share{Audience: artifacts.Audience{
				Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleViewer})
			return err
		},
		"unshare": func() error {
			_, err := a.Unshare(ctx, jane, id, artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"})
			return err
		},
		"transfer:bob": func() error { _, err := a.Transfer(ctx, jane, id, "bob"); return err },
		"link_work_item:ENG-12": func() error {
			_, err := a.LinkWorkItem(ctx, jane, id, "ENG-12")
			return err
		},
		"trash":   func() error { _, err := a.Trash(ctx, jane, id); return err },
		"restore": func() error { _, err := a.Restore(ctx, jane, id); return err },
		"purge":   func() error { _, err := a.Purge(ctx, jane, id); return err },
		"add_asset": func() error {
			_, _, err := a.AddAsset(ctx, jane, artifactsdomain.AssetInput{Artifact: id, Name: "a.png"})
			return err
		},
		"remove_asset":    func() error { _, err := a.RemoveAsset(ctx, jane, id, uuid.New()); return err },
		"pin:true":        func() error { _, err := a.Pin(ctx, jane, id, true); return err },
		"opened":          func() error { _, err := a.Opened(ctx, jane, id); return err },
		"read_elevated:4": func() error { _, err := a.ReadElevated(ctx, jane, id, 4); return err },
		"rename_folder":   func() error { _, err := a.RenameFolder(ctx, jane, "f-1", "Q3"); return err },
		"move_folder":     func() error { _, err := a.MoveFolder(ctx, jane, "f-1", "f-2"); return err },
		"remove_folder":   func() error { _, err := a.RemoveFolder(ctx, jane, "f-1"); return err },
		"move":            func() error { _, err := a.Move(ctx, jane, id, "f-2"); return err },
		"adopt_brand": func() error {
			_, err := a.AdoptBrand(ctx, jane, artifactsdomain.BrandAdoptInput{Version: 7})
			return err
		},
		"clear_brand": func() error { _, err := a.ClearBrand(ctx, jane); return err },
	} {
		if err := write(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, want := range []string{"create:jane", "publish:jane", "patch:jane", "share:jane",
		"unshare:jane", "transfer:bob:jane", "link_work_item:ENG-12:jane", "trash:jane",
		"restore:jane", "purge:jane", "add_asset:jane", "remove_asset:jane", "pin:true:jane", "opened:jane", "read_elevated:4:jane",
		"create_folders:Reports/Weekly:jane", "rename_folder:f-1:Q3:jane", "move_folder:f-1:f-2:jane",
		"remove_folder:f-1:jane", "move:f-2:jane",
		"adopt_brand:7:jane", "clear_brand:jane"} {
		found := false
		for _, got := range v.calls {
			found = found || got == want
		}
		if !found {
			t.Errorf("the serving node never ran %s, with the caller's authority: ran %v", want, v.calls)
		}
	}
	for i, key := range v.keys {
		if key != jane.OpKey {
			t.Errorf("write %s crossed with key %q, want the caller's %q", v.calls[i], key, jane.OpKey)
		}
	}
}

// A KEYED ARTIFACTS WRITE IS REPEATABLE AND AN UNKEYED ONE IS NOT: keyed, every
// record id derives from the caller's key, so a repeat on the next data node is
// the same operation; unkeyed, a repeat would be a second write.
func TestAnArtifactsWriteIsRepeatableExactlyWhenKeyed(t *testing.T) {
	t.Parallel()
	keyed := artifactsdomain.Call{OpKey: statelog.NewOpID(time.Now(), "")}
	if !opArtifactPublish.repeatable(artifactPublishArgs{Call: keyed}) {
		t.Error("a keyed publish is not repeatable")
	}
	if opArtifactPublish.repeatable(artifactPublishArgs{}) {
		t.Error("an unkeyed publish is repeatable, so an unanswered one would publish twice")
	}
	if opArtifactReadElevated.spec.class != opIdempotentWrite {
		t.Error("an elevated read, whose operation is always the day's, is not repeated")
	}
}
