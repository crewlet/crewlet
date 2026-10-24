package artifactsapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE RIG'S FOLDERS. `marketing` is jane's, shared with the company as
// contributor; `campaigns` (bob's) and `notebook` (scribe's, an agent lee
// leads) sit in it, and jane's `notes` is filed in `campaigns`. `drafts` is
// jane's own, shared with nobody; `inner` (jane's) and `lent` (bob's, moved
// there by an administrator) sit in it.
var (
	marketing = artifactsdomain.FolderIDOf("rig-folders", 0).String()
	campaigns = artifactsdomain.FolderIDOf("rig-folders", 1).String()
	notebook  = artifactsdomain.FolderIDOf("rig-folders", 2).String()
	drafts    = artifactsdomain.FolderIDOf("rig-folders", 3).String()
	inner     = artifactsdomain.FolderIDOf("rig-folders", 4).String()
	lent      = artifactsdomain.FolderIDOf("rig-folders", 5).String()
	nowhere   = artifactsdomain.FolderIDOf("rig-folders", 99).String()
)

// companyShares is the "Everyone" preset.
var companyShares = []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
	Role: artifacts.RoleContributor}}

// folders is every folder of the rig, live.
func folders() map[string]artifactsdomain.Folder {
	folder := func(id, parent, root, name, owner string, kind iam.ActorKind, rootOwner string,
		shares []artifacts.Share) artifactsdomain.Folder {
		return artifactsdomain.Folder{ID: id, Parent: parent, Root: root, Name: name, Owner: owner,
			OwnerKind: kind, RootOwner: rootOwner, RootOwnerKind: iam.ActorHuman, Shares: shares,
			Status: artifactsdomain.FolderLive}
	}
	return map[string]artifactsdomain.Folder{
		marketing: folder(marketing, "", marketing, "Marketing", "jane", iam.ActorHuman, "jane", companyShares),
		campaigns: folder(campaigns, marketing, marketing, "Campaigns", "bob", iam.ActorHuman, "jane", companyShares),
		notebook:  folder(notebook, marketing, marketing, "Notebook", "scribe", iam.ActorAgent, "jane", companyShares),
		drafts:    folder(drafts, "", drafts, "Drafts", "jane", iam.ActorHuman, "jane", nil),
		inner:     folder(inner, drafts, drafts, "Inner", "jane", iam.ActorHuman, "jane", nil),
		lent:      folder(lent, drafts, drafts, "Lent", "bob", iam.ActorHuman, "jane", nil),
	}
}

func (e *estate) Folder(_ context.Context, id string, _ statelog.Freshness) (
	artifactsdomain.Folder, statelog.Position, error) {
	if f, ok := folders()[id]; ok {
		return f, statelog.Position{}, nil
	}
	return artifactsdomain.Folder{}, statelog.Position{}, artifactsdomain.ErrFolderNotFound
}

// Folders answers EVERY folder, whoever asks: the estate's own filter is
// SQL's (artifactsdomain's tests hold it), and what this surface must never do
// is show a folder the authority table does not.
func (e *estate) Folders(context.Context, artifactsdomain.Viewer, statelog.Freshness) (
	artifactsdomain.FolderTree, error) {
	all := folders()
	tree := artifactsdomain.FolderTree{TopLevel: 3}
	for _, id := range []string{drafts, inner, lent, marketing, campaigns, notebook} {
		tree.Folders = append(tree.Folders, artifactsdomain.FolderItem{Folder: all[id], Artifacts: 1})
	}
	return tree, nil
}

func (w *writes) CreateFolders(_ context.Context, c artifactsdomain.Call, in artifactsdomain.FolderCreateInput) (
	artifactsdomain.FolderCreated, error) {
	written, err := w.record("create_folders", c, artifacts.ID{}, in)
	made := artifactsdomain.Folder{ID: nowhere, Parent: in.Parent, Root: nowhere, Name: in.Names[0],
		Owner: in.Owner, OwnerKind: in.OwnerKind, RootOwner: in.Owner, RootOwnerKind: in.OwnerKind,
		Status: artifactsdomain.FolderLive}
	return artifactsdomain.FolderCreated{Folders: []artifactsdomain.Folder{made}, Created: 1,
		Result: written.Result}, err
}
func (w *writes) RenameFolder(_ context.Context, c artifactsdomain.Call, id, name string) (
	artifactsdomain.Written, error) {
	return w.record("rename_folder", c, artifacts.ID{}, [2]string{id, name})
}
func (w *writes) MoveFolder(_ context.Context, c artifactsdomain.Call, id, parent string) (
	artifactsdomain.Written, error) {
	return w.record("move_folder", c, artifacts.ID{}, [2]string{id, parent})
}
func (w *writes) RemoveFolder(_ context.Context, c artifactsdomain.Call, id string) (
	artifactsdomain.Written, error) {
	return w.record("remove_folder", c, artifacts.ID{}, id)
}
func (w *writes) Move(_ context.Context, c artifactsdomain.Call, id artifacts.ID, folder string) (
	artifactsdomain.Written, error) {
	return w.record("move", c, id, folder)
}

// created is every create the pipeline was asked.
func (p *pipeline) created() []artifactupload.CreateRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]artifactupload.CreateRequest(nil), p.creates...)
}

// treeOf is the folder tree p is answered, by id: the surface's own question,
// which the app mounts as the named read `artifact_folders`.
func (g *rig) treeOf(p iam.Principal) map[string]map[string]any {
	g.t.Helper()
	answer, err := g.svc.Gate().Folders(context.Background(), g.estate, p, statelog.Freshness{})
	if err != nil {
		g.t.Fatalf("the folder tree: %v", err)
	}
	data, err := json.Marshal(answer)
	if err != nil {
		g.t.Fatal(err)
	}
	var decoded struct {
		Folders []map[string]any `json:"folders"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		g.t.Fatalf("the folder tree is not JSON: %s", data)
	}
	out := map[string]map[string]any{}
	for _, f := range decoded.Folders {
		out[f["id"].(string)] = f
	}
	return out
}

// A FOLDER IS SEEN BY ITS TOP-LEVEL FOLDER'S AUDIENCE AND NOBODY ELSE: bob,
// who is in the company, sees the company tree whole and none of jane's own;
// jane sees both. Each folder carries the reader's role, decided by the
// table, and its shares only to the reader who manages it.
//
// Mutation: decide a folder's visibility on its own owner (FolderManagerObject)
// and bob is shown `lent` under a `Drafts` he cannot see; decide its
// management on its top-level folder and jane is answered owner of bob's
// `campaigns`.
func TestAFolderIsSeenByItsTopLevelFoldersAudience(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	bob := g.treeOf(person("bob"))
	for _, id := range []string{marketing, campaigns, notebook} {
		if bob[id] == nil {
			t.Errorf("bob is not shown company folder %s", id)
		}
	}
	for _, id := range []string{drafts, inner, lent} {
		if bob[id] != nil {
			t.Errorf("bob is shown %s, in jane's own tree", id)
		}
	}
	if got := len(g.treeOf(person("jane"))); got != 6 {
		t.Errorf("jane is shown %d folders, want all 6", got)
	}

	for _, tc := range []struct {
		who  iam.Principal
		id   string
		role string
	}{
		{person("jane"), marketing, "owner"},
		{person("jane"), campaigns, "contributor"},
		{person("bob"), campaigns, "owner"},
		{person("bob"), marketing, "contributor"},
		{person("lee"), notebook, "owner"},
		{person("jane"), drafts, "owner"},
		{person("jane"), lent, "contributor"},
	} {
		f := g.treeOf(tc.who)[tc.id]
		if f == nil || f["role"] != tc.role {
			t.Errorf("%s's role on %s is %v, want %s", tc.who.Seat, tc.id, f["role"], tc.role)
			continue
		}
		held := len(folders()[tc.id].Shares) > 0
		if _, shown := f["shares"]; shown != (held && tc.role == "owner") {
			t.Errorf("%s (%s) on %s is shown the shares: %v", tc.who.Seat, tc.role, tc.id, shown)
		}
	}
}

// THE ADMIN PATH LISTS NO PRIVATE FOLDER: an administrator who relates to
// nothing in jane's own tree is shown none of it, as a listing never shows an
// artifact on the admin path, and the company's folders they manage as the
// admin path (recorded where a write uses it). Asking for the private folder by
// id is answered as a folder that is not there.
//
// Mutation: show a folder on authz.Decide's admission rather than
// authz.Visible and ada is shown Drafts by an override nobody recorded.
func TestTheAdminPathListsNoPrivateFolder(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	ada := person("ada", iam.GrantAdmin)
	tree := g.treeOf(ada)
	if tree[drafts] != nil || tree[inner] != nil {
		t.Error("the admin path listed a private folder")
	}
	if tree[marketing] == nil || tree[marketing]["role"] != "owner" {
		t.Errorf("an administrator is shown the company folder as %v", tree[marketing])
	}
	if w := g.do(ada, "PATCH", "/artifact-folders/"+drafts, `{"name":"Mine"}`, keyed()...); w.Code !=
		http.StatusNotFound {
		t.Errorf("an administrator renaming a private folder: %d %s", w.Code, w.Body)
	}
}

// EVERY VERB THE FOLDER SURFACE ASKS HAS ITS ROW, of the class the decision
// reads: the folder's verbs are decided per object, the create by the app.
func TestEveryFolderVerbHasItsRow(t *testing.T) {
	t.Parallel()
	for action, want := range map[authz.Action]authz.Class{
		authz.ActionArtifactFolderCreate: authz.ClassApp,
		authz.ActionArtifactMove:         authz.ClassShared,
		authz.ActionArtifactFolderFile:   authz.ClassShared,
		authz.ActionArtifactFolderRename: authz.ClassShared,
		authz.ActionArtifactFolderMove:   authz.ClassShared,
		authz.ActionArtifactFolderRemove: authz.ClassShared,
		authz.ActionArtifactFolderUnfile: authz.ClassShared,
	} {
		if class, ok := authz.ClassOf(action); !ok || class != want {
			t.Errorf("%s is decided by %v (row: %v), want %v", action, class, ok, want)
		}
	}
}

// AN INVISIBLE FOLDER IS NOT FOUND LIKE AN UNKNOWN ONE, on every route that
// names one in its path and every route that names one in its body: bob
// renaming, moving or removing jane's own folder is answered exactly as for a
// folder id nothing holds, and filing into it as filing into nothing.
//
// Mutation: answer a folder bob cannot see with the role refusal its
// management decision makes, and the bodies differ.
func TestAnInvisibleFolderIsNotFoundLikeAnUnknownOne(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	bob := person("bob")
	for _, tc := range []struct{ method, rest, body string }{
		{"PATCH", "", `{"name":"Mine"}`},
		{"PATCH", "", `{"parent":""}`},
		{"DELETE", "", ``},
	} {
		hidden := g.do(bob, tc.method, "/artifact-folders/"+drafts+tc.rest, tc.body, keyed()...)
		absent := g.do(bob, tc.method, "/artifact-folders/"+nowhere+tc.rest, tc.body, keyed()...)
		if hidden.Code != http.StatusNotFound || hidden.Body.String() != absent.Body.String() {
			t.Errorf("%s on a folder bob cannot see: %d %s, and on none: %d %s", tc.method,
				hidden.Code, hidden.Body, absent.Code, absent.Body)
		}
	}
	sam := person("sam")
	hidden := g.do(sam, "PUT", at(page, "/folder"), `{"folder":"`+inner+`"}`, keyed()...)
	absent := g.do(sam, "PUT", at(page, "/folder"), `{"folder":"`+nowhere+`"}`, keyed()...)
	if hidden.Code != http.StatusNotFound || errorOf(t, hidden) != "folder_not_found" ||
		hidden.Body.String() != absent.Body.String() {
		t.Errorf("filing into a folder sam cannot see: %d %s, and into none: %d %s", hidden.Code,
			hidden.Body, absent.Code, absent.Body)
	}
	if c := g.writes.all(); len(c) != 0 {
		t.Errorf("a refused folder gesture wrote %+v", c)
	}
}

// FILING ASKS THE FOLDER AND THE ARTIFACT: editor on the artifact AND
// contributor on the destination, and a move writes nothing but the move
// (a folder grants nothing, so no share rides it).
//
// Mutation: drop the destination's decision and sam files jane's page into
// her own folder; drop the artifact's and bob, a viewer, re-files jane's notes.
func TestFilingAsksTheFolderAndTheArtifact(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	if w := g.do(person("bob"), "PUT", at(notes, "/folder"), `{"folder":"`+marketing+`"}`, keyed()...); w.Code !=
		http.StatusForbidden {
		t.Errorf("a viewer of the artifact filing it: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("sam"), "PUT", at(page, "/folder"), `{"folder":"`+drafts+`"}`, keyed()...); w.Code !=
		http.StatusNotFound {
		t.Errorf("an editor filing into a folder they cannot see: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("sam"), "PUT", at(page, "/folder"), `{"folder":"`+campaigns+`"}`, keyed()...); w.Code !=
		http.StatusOK {
		t.Fatalf("an editor filing into a company folder: %d %s", w.Code, w.Body)
	}
	c := g.writes.all()
	if len(c) != 1 || c[0].verb != "move" || c[0].id != page || c[0].arg != campaigns {
		t.Fatalf("the filing wrote %+v", c)
	}
	if c[0].c.Authority.Role != artifacts.RoleEditor || c[0].c.Authority.Elevated {
		t.Errorf("the move is stamped %+v", c[0].c.Authority)
	}
}

// THE FOLDER'S MANAGER TAKES OUT WHAT THEY CAN READ: bob, who owns
// `campaigns` and only views jane's notes filed in it, moves the notes to the
// top level, and cannot file them anywhere else.
func TestAFoldersManagerUnfilesWhatTheyCanRead(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	bob := person("bob")
	if w := g.do(bob, "PUT", at(notes, "/folder"), `{"folder":""}`, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("the folder's owner unfiling: %d %s", w.Code, w.Body)
	}
	if w := g.do(bob, "PUT", at(notes, "/folder"), `{"folder":"`+marketing+`"}`, keyed()...); w.Code !=
		http.StatusForbidden {
		t.Errorf("the folder's owner re-filing an artifact they only view: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("eve"), "PUT", at(notes, "/folder"), `{"folder":""}`, keyed()...); w.Code !=
		http.StatusNotFound {
		t.Errorf("somebody with no relation to the artifact unfiling it: %d %s", w.Code, w.Body)
	}
	c := g.writes.all()
	if len(c) != 1 || c[0].verb != "move" || c[0].arg != "" || c[0].c.Authority.Role != artifacts.RoleViewer {
		t.Errorf("the unfiling wrote %+v", c)
	}
}

// MANAGING A FOLDER IS ITS OWNER'S, THE OWNER'S LEAD'S OR AN ADMINISTRATOR'S,
// AND A PERSON'S: bob is refused renaming jane's folder naming the role it
// needs, lee renames the agent's folder he leads, the agent itself is refused,
// and an administrator's rename is stamped elevated.
func TestManagingAFolderIsItsOwnersAndAPersons(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "PATCH", "/artifact-folders/"+marketing, `{"name":"Ours"}`, keyed()...)
	if w.Code != http.StatusForbidden || fields(t, w)["role"] != "owner" {
		t.Errorf("a contributor renaming the folder: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("lee"), "PATCH", "/artifact-folders/"+notebook, `{"name":"Log"}`, keyed()...); w.Code !=
		http.StatusOK {
		t.Errorf("the lead renaming the agent's folder: %d %s", w.Code, w.Body)
	}
	scribe := iam.Principal{Login: "scribe", Kind: iam.KindSeat, Seat: "scribe", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantArtifacts}}
	if w := g.do(scribe, "DELETE", "/artifact-folders/"+notebook, ``, keyed()...); w.Code != http.StatusForbidden {
		t.Errorf("an agent removing its own folder: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("ada", iam.GrantAdmin), "PATCH", "/artifact-folders/"+marketing, `{"name":"Brand"}`,
		keyed()...); w.Code != http.StatusOK {
		t.Fatalf("an administrator renaming a company folder: %d %s", w.Code, w.Body)
	}
	c := g.writes.all()
	if len(c) != 2 || c[0].verb != "rename_folder" || c[0].c.Authority.Elevated || !c[1].c.Authority.Elevated {
		t.Errorf("the renames were written %+v", c)
	}
}

// A MOVE THAT WIDENS IS CONFIRMED: jane moving her own `inner` under the
// company's `marketing` makes it visible by name to everyone, so it is refused
// until she confirms it; a move within one tree asks nothing.
func TestAMoveThatWidensIsConfirmed(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	jane := person("jane")
	w := g.do(jane, "PATCH", "/artifact-folders/"+inner, `{"parent":"`+marketing+`"}`, keyed()...)
	if w.Code != http.StatusConflict || errorOf(t, w) != "folder_widens" {
		t.Fatalf("a widening move unconfirmed: %d %s", w.Code, w.Body)
	}
	if len(g.writes.all()) != 0 {
		t.Fatal("an unconfirmed widening move was written")
	}
	w = g.do(jane, "PATCH", "/artifact-folders/"+inner, `{"name":"Shared ideas","parent":"`+marketing+`"}`,
		keyed()...)
	if w.Code != http.StatusConflict || len(g.writes.all()) != 0 {
		t.Fatalf("a rename sent with an unconfirmed widening move: %d %s, writes %+v", w.Code, w.Body,
			g.writes.all())
	}
	if w := g.do(jane, "PATCH", "/artifact-folders/"+inner, `{"parent":"`+marketing+`","confirm_widen":true}`,
		keyed()...); w.Code != http.StatusOK {
		t.Fatalf("a confirmed widening move: %d %s", w.Code, w.Body)
	}
	if w := g.do(person("bob"), "PATCH", "/artifact-folders/"+campaigns, `{"parent":""}`, keyed()...); w.Code !=
		http.StatusOK {
		t.Errorf("a company folder's move to the top level, which keeps its audience: %d %s", w.Code, w.Body)
	}
	if w := g.do(jane, "PATCH", "/artifact-folders/"+marketing, `{"parent":"`+drafts+`"}`, keyed()...); w.Code !=
		http.StatusOK {
		t.Errorf("a move that only narrows who sees it: %d %s", w.Code, w.Body)
	}
	if c := g.writes.all(); len(c) != 3 {
		t.Errorf("the moves were written %+v", c)
	}
}

// A CREATE FILES INTO A FOLDER ONLY WHERE ITS CREATOR MAY FILE: bob creating
// into jane's own folder is answered as a folder that is not there, before the
// pipeline is asked anything.
func TestACreateIntoAFolderAsksTheFolder(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"kind":"html","title":"Plan","upload":"u-1","folder":"` + drafts + `"}`
	if w := g.do(person("bob"), "POST", "/artifacts", body, keyed()...); w.Code != http.StatusNotFound ||
		errorOf(t, w) != "folder_not_found" {
		t.Errorf("a create into a folder bob cannot see: %d %s", w.Code, w.Body)
	}
	if n := len(g.pipeline.created()); n != 0 {
		t.Errorf("the pipeline was asked %d creates", n)
	}
	body = `{"kind":"html","title":"Plan","upload":"u-1","folder":"` + campaigns + `"}`
	if w := g.do(person("bob"), "POST", "/artifacts", body, keyed()...); w.Code != http.StatusCreated {
		t.Fatalf("a create into a company folder: %d %s", w.Code, w.Body)
	}
	if got := g.pipeline.created(); len(got) != 1 || got[0].Folder != campaigns {
		t.Errorf("the pipeline was asked %+v", got)
	}
}

// A FOLDER CREATE ASKS THE PARENT, AND A TOP-LEVEL ONE STATES WHO SEES IT: bob
// creates inside the company folder, is refused inside jane's own, and the
// writer is handed the caller as owner.
func TestAFolderCreateAsksItsParent(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	bob := person("bob")
	if w := g.do(bob, "POST", "/artifact-folders", `{"name":"Q3","parent":"`+inner+`"}`, keyed()...); w.Code !=
		http.StatusNotFound || errorOf(t, w) != "folder_not_found" {
		t.Errorf("a create inside a folder bob cannot see: %d %s", w.Code, w.Body)
	}
	if w := g.do(bob, "POST", "/artifact-folders", `{"path":"Q3/Week 1","parents":true,"parent":"`+
		marketing+`"}`, keyed()...); w.Code != http.StatusCreated {
		t.Fatalf("a create inside the company folder: %d %s", w.Code, w.Body)
	}
	if w := g.do(bob, "POST", "/artifact-folders", `{"name":"A","path":"B"}`, keyed()...); w.Code !=
		http.StatusUnprocessableEntity {
		t.Errorf("a create naming a name and a path: %d %s", w.Code, w.Body)
	}
	c := g.writes.all()
	if len(c) != 1 {
		t.Fatalf("the creates were written %+v", c)
	}
	in := c[0].arg.(artifactsdomain.FolderCreateInput)
	if in.Owner != "bob" || in.Parent != marketing || !slices.Equal(in.Names, []string{"Q3", "Week 1"}) ||
		!in.Parents || c[0].c.Authority.Role != artifacts.RoleContributor {
		t.Errorf("the create was written %+v by %+v", in, c[0].c.Authority)
	}
}

// A BULK MOVE DECIDES EACH ITEM ALONE and names every refusal: sam files the
// page he edits, and the notes he does not relate to and an artifact nothing
// holds are each refused in their own result, with nothing written for them.
func TestABulkMoveDecidesEachItemAlone(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	body := `{"items":[{"artifact":"` + page.String() + `","folder":"` + campaigns + `"},` +
		`{"artifact":"` + notes.String() + `","folder":"` + campaigns + `"},` +
		`{"artifact":"` + unknown.String() + `","folder":"` + campaigns + `"},` +
		`{"artifact":"` + page.String() + `","folder":"` + inner + `"}]}`
	w := g.do(person("sam"), "POST", "/artifacts/folder-moves", body, keyed()...)
	if w.Code != http.StatusOK {
		t.Fatalf("a bulk move: %d %s", w.Code, w.Body)
	}
	var answer struct {
		Results []struct{ Artifact, Outcome, Message string } `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	want := []string{"moved", "not_found", "not_found", "folder_not_found"}
	if len(answer.Results) != len(want) {
		t.Fatalf("the bulk move answered %+v", answer.Results)
	}
	for i, r := range answer.Results {
		if r.Outcome != want[i] || (r.Outcome != "moved" && r.Message == "") {
			t.Errorf("item %d answered %+v, want %s with a message", i, r, want[i])
		}
	}
	if c := g.writes.all(); len(c) != 1 || c[0].id != page {
		t.Errorf("the bulk move wrote %+v", c)
	}
	many := `{"items":[` + string(bytes.Repeat([]byte(`{"artifact":"`+page.String()+`","folder":""},`),
		artifacts.MaxMovesPerCall)) + `{"artifact":"` + page.String() + `","folder":""}]}`
	if w := g.do(person("sam"), "POST", "/artifacts/folder-moves", many, keyed()...); w.Code !=
		http.StatusUnprocessableEntity {
		t.Errorf("a bulk move past its bound: %d %s", w.Code, w.Body)
	}
}
