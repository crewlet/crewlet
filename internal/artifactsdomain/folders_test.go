package artifactsdomain_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FOLDER TREE, written through the shipped writer and applied by the
// shipped applier on a real broker (the harness), and the applier alone over
// records formed by hand (the estate) for what no well-behaved writer forms.

// mkdir creates names under parent ("" for the top level) as c, with the
// visibility a top-level folder states, and answers the chain.
func (n *node) mkdir(c artifactsdomain.Call, parent string, visibility artifactsdomain.FolderVisibility,
	names ...string) []artifactsdomain.Folder {
	n.t.Helper()
	out, err := n.writer.CreateFolders(n.t.Context(), c, artifactsdomain.FolderCreateInput{
		Parent: parent, Names: names, Parents: len(names) > 1, Visibility: visibility,
		Owner: c.Authority.Actor.Name, OwnerKind: c.Authority.Actor.Kind,
	})
	if err != nil {
		n.t.Fatalf("create %v under %q: %v", names, parent, err)
	}
	return out.Folders
}

// createIn makes an HTML artifact owned by c's actor, filed in folder.
func (n *node) createIn(c artifactsdomain.Call, title, folder string) artifacts.ID {
	n.t.Helper()
	got, err := n.writer.Create(n.t.Context(), c, artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: title, Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Folder: folder,
		First: page(upload(n.t, "index.html", "<p>"+title+"</p>")),
	})
	if err != nil {
		n.t.Fatalf("create %q in %q: %v", title, folder, err)
	}
	return got.Artifact
}

// shareWith shares artifact id with the whole company as viewer.
func (n *node) shareWithCompany(c artifactsdomain.Call, id artifacts.ID) {
	n.t.Helper()
	if _, err := n.writer.Share(n.t.Context(), c, id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceCompany}, Role: artifacts.RoleViewer,
	}); err != nil {
		n.t.Fatalf("share %s with the company: %v", id, err)
	}
}

// folder reads one folder back.
func (n *node) folder(id string) artifactsdomain.Folder {
	n.t.Helper()
	f, _, err := n.reader.Folder(n.t.Context(), id, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		n.t.Fatalf("read folder %s: %v", id, err)
	}
	return f
}

// count is the rows a query counts on the node.
func (n *node) count(query string, args ...any) int {
	n.t.Helper()
	var out int
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), query, args...).Scan(&out)
	}); err != nil {
		n.t.Fatalf("count %q: %v", query, err)
	}
	return out
}

// A CHAIN IS CREATED IN ONE RECORD, AND A RETRY NAMES THE SAME FOLDERS: a path
// with its missing parents is one record whose ids its operation derives, so
// the retry an unknown outcome asks for answers the folders the first attempt
// made rather than making more; a path already in place publishes nothing; and
// one name is refused where its place already holds it, whatever its case.
//
// Mutation: mint folder ids freshly (uuid.New) and the retry answers other
// folders; fold no case into the key and "marketing" is created beside
// "Marketing".
func TestAFolderChainIsOneRecordAndARetryNamesTheSameFolders(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	jane.OpKey = statelog.NewOpID(time.Now(), "")
	in := artifactsdomain.FolderCreateInput{Names: []string{"Marketing", "Q3", "Week 1"}, Parents: true,
		Visibility: artifactsdomain.FolderCompany, Owner: "jane", OwnerKind: iam.ActorHuman}
	first, err := n.writer.CreateFolders(t.Context(), jane, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Created != 3 || len(first.Folders) != 3 {
		t.Fatalf("the chain answered %d folders, %d created", len(first.Folders), first.Created)
	}
	for i, f := range first.Folders {
		if f.Depth != i+1 || f.Root != first.Folders[0].ID || f.Visibility != artifactsdomain.FolderCompany ||
			(i > 0 && f.Parent != first.Folders[i-1].ID) || f.RootOwner != "jane" {
			t.Errorf("folder %d of the chain is %+v", i, f)
		}
	}
	again, err := n.writer.CreateFolders(t.Context(), jane, in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(again.Folders, first.Folders, func(a, b artifactsdomain.Folder) bool { return a.ID == b.ID }) ||
		again.Created != 3 {
		t.Errorf("the retry answered %+v, want the first attempt's folders", again)
	}
	jane.OpKey = ""
	week2 := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Marketing", "Q3", "Week 2")
	if len(week2) != 3 || week2[1].ID != first.Folders[1].ID || week2[2].ID == first.Folders[2].ID {
		t.Errorf("a path whose parents exist reused %+v", week2)
	}
	whole, err := n.writer.CreateFolders(t.Context(), jane, artifactsdomain.FolderCreateInput{
		Names: []string{"marketing", "Q3"}, Parents: true, Visibility: artifactsdomain.FolderCompany,
		Owner: "jane", OwnerKind: iam.ActorHuman})
	if err != nil || whole.Created != 0 || !whole.Result.Position.IsZero() || len(whole.Folders) != 2 {
		t.Errorf("a path already in place answered %+v (%v), want the chain and nothing published", whole, err)
	}
	for _, name := range []string{"Marketing", "MARKETING"} {
		_, err := n.writer.CreateFolders(t.Context(), jane, artifactsdomain.FolderCreateInput{
			Names: []string{name}, Visibility: artifactsdomain.FolderCompany, Owner: "jane",
			OwnerKind: iam.ActorHuman})
		if !errors.Is(err, artifactsdomain.ErrFolderNameTaken) {
			t.Errorf("a second company folder %q: %v, want the name taken", name, err)
		}
	}
	if got := n.count(`SELECT COUNT(*) FROM artifact_folder_history WHERE op = 'create'`); got != 2 {
		t.Errorf("the folder history holds %d creates, want the two that made folders", got)
	}
}

// TOP-LEVEL NAMES HAVE TWO NAMESPACES, so a name is never an oracle: each
// owner's own top-level names are unique among their own, the company's among
// the company's, and a private folder may share a company folder's name, which
// a path naming both then refuses naming each rather than guessing.
func TestTopLevelNamesHaveTwoNamespaces(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, bob := person("jane", artifacts.RoleOwner), person("bob", artifacts.RoleOwner)
	janes := n.mkdir(jane, "", artifactsdomain.FolderPrivate, "Drafts")[0]
	n.mkdir(bob, "", artifactsdomain.FolderPrivate, "Drafts")
	company := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Drafts")[0]
	if _, err := n.writer.CreateFolders(t.Context(), jane, artifactsdomain.FolderCreateInput{
		Names: []string{"drafts"}, Visibility: artifactsdomain.FolderPrivate, Owner: "jane",
		OwnerKind: iam.ActorHuman}); !errors.Is(err, artifactsdomain.ErrFolderNameTaken) {
		t.Errorf("jane's second private Drafts: %v", err)
	}
	var ambiguous *artifactsdomain.FolderAmbiguous
	_, err := n.reader.ResolveFolder(t.Context(), "Drafts", []artifactsdomain.Viewer{{Record: "jane"}},
		statelog.Freshness{})
	if !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Fatalf("jane resolving Drafts: %v, want both of hers named", err)
	}
	ids := []string{ambiguous.Candidates[0].ID, ambiguous.Candidates[1].ID}
	if !slices.Contains(ids, janes.ID) || !slices.Contains(ids, company.ID) {
		t.Errorf("the candidates are %v", ids)
	}
	got, err := n.reader.ResolveFolder(t.Context(), "drafts/", []artifactsdomain.Viewer{{Record: "sam"}},
		statelog.Freshness{})
	if err != nil || got.ID != company.ID {
		t.Errorf("sam resolving Drafts: %+v (%v), want the company's", got, err)
	}
	if got, err := n.reader.ResolveFolder(t.Context(), janes.ID, []artifactsdomain.Viewer{{Record: "jane"}},
		statelog.Freshness{}); err != nil || got.ID != janes.ID {
		t.Errorf("jane resolving her folder by id: %+v (%v)", got, err)
	}
}

// AN AGENT CREATES SUBFOLDERS ONLY, AND A TOP-LEVEL FOLDER STATES WHO SEES IT:
// a company-wide top-level folder is a company-wide audience, which an agent
// never grants (as it never shares an artifact with the company), and a
// subfolder is seen by its tree, so it states nothing.
//
// Mutation: drop the agent rule from checkFolderCreate and the agent's
// top-level company folder is written.
func TestAnAgentNeverSharesAFolderWithTheCompany(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane, scribe := person("jane", artifacts.RoleOwner), agent("agent-scribe", artifacts.RoleContributor)
	reports := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Reports")[0]
	for name, in := range map[string]artifactsdomain.FolderCreateInput{
		"an agent's top-level folder": {Names: []string{"Mine"}, Visibility: artifactsdomain.FolderCompany,
			Owner: "agent-scribe", OwnerKind: iam.ActorAgent},
		"an agent's private top-level folder": {Names: []string{"Mine"}, Visibility: artifactsdomain.FolderPrivate,
			Owner: "agent-scribe", OwnerKind: iam.ActorAgent},
		"a subfolder stating a visibility": {Parent: reports.ID, Names: []string{"Weekly"},
			Visibility: artifactsdomain.FolderCompany, Owner: "agent-scribe", OwnerKind: iam.ActorAgent},
	} {
		if _, err := n.writer.CreateFolders(t.Context(), scribe, in); !errors.Is(err, artifactsdomain.ErrInvalid) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}
	if _, err := n.writer.CreateFolders(t.Context(), jane, artifactsdomain.FolderCreateInput{
		Names: []string{"Untold"}, Owner: "jane", OwnerKind: iam.ActorHuman}); !errors.Is(err,
		artifactsdomain.ErrInvalid) {
		t.Errorf("a top-level folder stating no visibility: %v", err)
	}
	weekly := n.mkdir(scribe, reports.ID, "", "Weekly")[0]
	if weekly.Owner != "agent-scribe" || weekly.Visibility != artifactsdomain.FolderCompany ||
		len(weekly.Shares) != 1 || weekly.RootOwner != "jane" {
		t.Errorf("the agent's subfolder is %+v", weekly)
	}
	if got := n.count(`SELECT COUNT(*) FROM artifact_folder_shares WHERE folder_id = ?`, weekly.ID); got != 0 {
		t.Errorf("a subfolder holds %d shares of its own", got)
	}
}

// DEPTH IS BOUNDED BY THE STAMP, in the writer from its snapshot and in the
// applier from the record: a chain, a create beneath a deep folder and a move
// that would carry a subtree past the bound are each refused.
//
// Mutation: check the applier's depth against its own constant and the hand
// record stamped 2 applies at depth 3.
func TestDepthIsBoundedByTheStamp(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	deep := make([]string, artifacts.MaxFolderDepth+1)
	for i := range deep {
		deep[i] = "L" + string(rune('1'+i))
	}
	if _, err := n.writer.CreateFolders(t.Context(), jane, artifactsdomain.FolderCreateInput{
		Names: deep, Parents: true, Visibility: artifactsdomain.FolderCompany, Owner: "jane",
		OwnerKind: iam.ActorHuman}); !errors.Is(err, artifactsdomain.ErrFolderDepth) {
		t.Errorf("a chain past the bound: %v", err)
	}
	chain := n.mkdir(jane, "", artifactsdomain.FolderCompany, deep[:artifacts.MaxFolderDepth]...)
	if _, err := n.writer.CreateFolders(t.Context(), jane, artifactsdomain.FolderCreateInput{
		Parent: chain[len(chain)-1].ID, Names: []string{"Too deep"}, Owner: "jane",
		OwnerKind: iam.ActorHuman}); !errors.Is(err, artifactsdomain.ErrFolderDepth) {
		t.Errorf("a create beneath the deepest folder: %v", err)
	}
	other := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Other", "Inside")
	if _, err := n.writer.MoveFolder(t.Context(), jane, other[0].ID, chain[2].ID); !errors.Is(err,
		artifactsdomain.ErrFolderDepth) {
		t.Errorf("a move carrying a subtree past the bound: %v", err)
	}

	e := newEstate(t)
	top := artifactsdomain.FolderIDOf("hand-depth", 0).String()
	e.apply(handFolderCreate(t, "hand-depth", "", 9, "A", "B"), monday)
	e.apply(handFolderCreate(t, "hand-deeper", artifactsdomain.FolderIDOf("hand-depth", 1).String(), 2, "C"),
		monday)
	if outcome, reason := e.folderVerdict("hand-deeper"); outcome != "refused" || reason != "folder_depth" {
		t.Errorf("a create past its stamp applied as %s %s", outcome, reason)
	}
	if got := e.count(`SELECT COUNT(*) FROM artifact_folders WHERE root = ?`, top); got != 2 {
		t.Errorf("the tree holds %d folders after the refused create, want 2", got)
	}
}

// A FOLDER CYCLE IS REFUSED WHERE IT IS APPLIED: the writer refuses a move into
// the folder itself or beneath it, and a record that tried it anyway is
// refused by every applier, the rows untouched.
//
// Mutation: drop the cycle check in applyFolderMove and the hand record makes
// A its own grandchild, a loop no walk ends.
func TestAFolderCycleIsRefusedWhereItIsApplied(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	chain := n.mkdir(jane, "", artifactsdomain.FolderCompany, "A", "B", "C")
	for _, into := range []string{chain[0].ID, chain[2].ID} {
		if _, err := n.writer.MoveFolder(t.Context(), jane, chain[0].ID, into); !errors.Is(err,
			artifactsdomain.ErrFolderCycle) {
			t.Errorf("moving A into %s: %v", into, err)
		}
	}

	e := newEstate(t)
	e.apply(handFolderCreate(t, "hand-cycle", "", artifacts.MaxFolderDepth, "A", "B"), monday)
	a, b := artifactsdomain.FolderIDOf("hand-cycle", 0).String(), artifactsdomain.FolderIDOf("hand-cycle", 1).String()
	e.apply(handRecord(t, artifactsdomain.FolderSubject(), artifactsdomain.OpMove, "hand-loop",
		artifactsdomain.ScopeSet{Subject: true}, artifactsdomain.FolderMovePayload{
			V: 1, Folder: a, Parent: b, Root: a, MaxDepth: artifacts.MaxFolderDepth,
			MaxChildren: artifacts.MaxFolderChildren, Authority: suiteAuthority(),
		}), monday)
	if outcome, reason := e.folderVerdict("hand-loop"); outcome != "refused" || reason != "folder_cycle" {
		t.Errorf("the loop applied as %s %s", outcome, reason)
	}
	if got := e.count(`SELECT COUNT(*) FROM artifact_folders WHERE id = ? AND parent = ''`, a); got != 1 {
		t.Error("the refused move moved A")
	}
}

// RENAMING KEEPS SIBLINGS APART BY THEIR FOLD: a case-only rename is a rename,
// a rename to a sibling's name is refused, and a rename to the name it has
// publishes nothing.
func TestARenameIsHeldToItsSiblings(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	plans := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Plans")[0]
	n.mkdir(jane, "", artifactsdomain.FolderCompany, "Notes")
	if _, err := n.writer.RenameFolder(t.Context(), jane, plans.ID, "PLANS"); err != nil {
		t.Fatalf("a case-only rename: %v", err)
	}
	if got := n.folder(plans.ID).Name; got != "PLANS" {
		t.Errorf("the folder is named %q", got)
	}
	if _, err := n.writer.RenameFolder(t.Context(), jane, plans.ID, "notes"); !errors.Is(err,
		artifactsdomain.ErrFolderNameTaken) {
		t.Errorf("a rename to a sibling's name: %v", err)
	}
	same, err := n.writer.RenameFolder(t.Context(), jane, plans.ID, "PLANS")
	if err != nil || !same.Result.Position.IsZero() {
		t.Errorf("a rename to the name it has: %+v (%v)", same, err)
	}
	if _, err := n.writer.RenameFolder(t.Context(), jane, plans.ID, "A/B"); !errors.Is(err,
		artifactsdomain.ErrInvalid) || !errors.Is(err, foldername.ErrInvalid) {
		t.Errorf("a name holding a slash: %v", err)
	}
}

// FILING NEVER MOVES THE ACCESS EPOCH: a move changes where the artifact is
// filed and nothing about who may open it (its shares, its epoch, which every
// view ticket carries), and leaves its `updated_at` alone (a move is not an
// edit). It is told to listeners as a folder movement, which narrows nothing;
// a move to where it already is writes nothing, and a trashed artifact is
// filed nowhere.
//
// Mutation: touch the head in applyMove and the epoch or the instant moves.
func TestFilingNeverMovesTheAccessEpoch(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	folder := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Plans")[0]
	id := n.create(jane, "Plan", upload(t, "index.html", "<p>plan</p>")).Artifact
	if _, err := n.writer.Share(t.Context(), jane, id, artifacts.Share{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: "bob"}, Role: artifacts.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	before := n.head(id)
	n.moved = nil
	if _, err := n.writer.Move(t.Context(), jane, id, folder.ID); err != nil {
		t.Fatalf("file it: %v", err)
	}
	after := n.head(id)
	switch {
	case after.Folder != folder.ID:
		t.Errorf("the artifact is filed in %q", after.Folder)
	case after.AccessEpoch != before.AccessEpoch || !slices.Equal(after.Shares, before.Shares):
		t.Errorf("filing changed its access: epoch %d to %d, shares %v to %v", before.AccessEpoch,
			after.AccessEpoch, before.Shares, after.Shares)
	case !after.UpdatedAt.Equal(before.UpdatedAt):
		t.Errorf("filing moved updated_at from %v to %v", before.UpdatedAt, after.UpdatedAt)
	}
	if !slices.ContainsFunc(n.moved, func(m artifactsdomain.Moved) bool {
		return m.Artifact == id && m.What == artifactsdomain.MoveFolder && m.AccessEpoch == before.AccessEpoch
	}) {
		t.Errorf("the move was not told: %+v", n.moved)
	}
	rows := n.count(`SELECT COUNT(*) FROM artifact_history WHERE artifact_id = ?`, id.String())
	if _, err := n.writer.Move(t.Context(), jane, id, folder.ID); err != nil {
		t.Fatal(err)
	}
	if got := n.count(`SELECT COUNT(*) FROM artifact_history WHERE artifact_id = ?`, id.String()); got != rows {
		t.Error("a move to where it already is wrote a record")
	}
	if _, err := n.writer.Move(t.Context(), jane, id, ""); err != nil || n.head(id).Folder != "" {
		t.Errorf("filing it at the top level: %v", err)
	}
	if _, err := n.writer.Trash(t.Context(), jane, id); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Move(t.Context(), jane, id, folder.ID); !errors.Is(err, artifactsdomain.ErrTrashed) {
		t.Errorf("filing a trashed artifact: %v", err)
	}
}

// A MOVE ROW NAMES NO FOLDER: the history row a move writes names the folders
// by id alone, so a reader of the artifact (who reads its history) is never
// told the name of a folder they cannot see.
//
// Mutation: write the folder's name into the summary and the row carries
// "Layoffs Q1" to every viewer of the artifact.
func TestAMoveRowNamesNoFolder(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	secret := n.mkdir(jane, "", artifactsdomain.FolderPrivate, "Layoffs Q1")[0]
	open := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Open plans")[0]
	id := n.create(jane, "Plan", upload(t, "index.html", "<p>plan</p>")).Artifact
	for _, to := range []string{secret.ID, open.ID} {
		if _, err := n.writer.Move(t.Context(), jane, id, to); err != nil {
			t.Fatal(err)
		}
	}
	page, err := n.reader.History(t.Context(), id, "", 2)
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("the history is %+v (%v)", page.Entries, err)
	}
	newest, older := page.Entries[0], page.Entries[1]
	if newest.Op != string(artifactsdomain.OpMove) || newest.Summary.FromFolder != secret.ID ||
		newest.Summary.ToFolder != open.ID || older.Summary.FromFolder != "" || older.Summary.ToFolder != secret.ID {
		t.Errorf("the move rows say %+v then %+v", older.Summary, newest.Summary)
	}
	for _, e := range page.Entries {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), "Layoffs") || strings.Contains(string(raw), "Open plans") {
			t.Errorf("a move's row names a folder: %s", raw)
		}
	}
}

// REMOVING A FOLDER MOVES WHAT IT HOLDS UP, in one record: its child folders
// and every artifact in it, live and trashed, move to its parent, the folder
// becomes a tombstone, its own history row counts what moved, and no
// artifact's history gains a row (nothing about any of them changed but where
// it is filed). A child whose name its new place already holds is refused
// first; a top-level folder's children become top-level folders keeping its
// audience.
//
// Mutation: cascade the removal to the artifacts (trash them) and the live one
// is trashed; skip the re-parent of child folders and Week 1 hangs under a
// tombstone, out of every tree.
func TestDeletingAFolderReparentsWhatItHolds(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	chain := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Marketing", "Q3", "Week 1")
	marketing, q3, week := chain[0], chain[1], chain[2]
	live := n.createIn(jane, "Live", q3.ID)
	binned := n.createIn(jane, "Binned", q3.ID)
	if _, err := n.writer.Trash(t.Context(), jane, binned); err != nil {
		t.Fatal(err)
	}
	beforeLive := n.head(live)
	histories := n.count(`SELECT COUNT(*) FROM artifact_history`)
	if _, err := n.writer.RemoveFolder(t.Context(), jane, q3.ID); err != nil {
		t.Fatalf("remove Q3: %v", err)
	}
	if got := n.folder(q3.ID); got.Status != artifactsdomain.FolderRemoved {
		t.Errorf("Q3 is %s after its removal", got.Status)
	}
	if got := n.folder(week.ID); got.Parent != marketing.ID || got.Depth != 2 {
		t.Errorf("Week 1 is under %q at depth %d, want Marketing's at 2", got.Parent, got.Depth)
	}
	for _, id := range []artifacts.ID{live, binned} {
		if got := n.head(id).Folder; got != marketing.ID {
			t.Errorf("%s is filed in %q after its folder's removal", id, got)
		}
	}
	if h := n.head(live); h.Status != artifactsdomain.StatusLive || !h.UpdatedAt.Equal(beforeLive.UpdatedAt) {
		t.Errorf("the live artifact is %s, updated %v (was %v)", h.Status, h.UpdatedAt, beforeLive.UpdatedAt)
	}
	if got := n.count(`SELECT COUNT(*) FROM artifact_history`); got != histories {
		t.Errorf("the removal wrote %d artifact history rows", got-histories)
	}
	var raw string
	if err := n.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT summary FROM artifact_folder_history
			WHERE folder_id = ? AND op = 'remove'`, q3.ID).Scan(&raw)
	}); err != nil {
		t.Fatal(err)
	}
	var summary artifactsdomain.FolderHistorySummary
	if err := json.Unmarshal([]byte(raw), &summary); err != nil || summary.Folders != 1 ||
		summary.Artifacts != 2 || summary.Parent != marketing.ID {
		t.Errorf("the removal's row says %s", raw)
	}

	plans := n.mkdir(jane, marketing.ID, "", "Plans")[0]
	inner := n.mkdir(jane, week.ID, "", "Plans")[0]
	if _, err := n.writer.RemoveFolder(t.Context(), jane, week.ID); !errors.Is(err,
		artifactsdomain.ErrFolderNameTaken) || !strings.Contains(err.Error(), `"Plans"`) {
		t.Errorf("a removal moving a child up onto a sibling's name: %v", err)
	}
	if _, err := n.writer.RenameFolder(t.Context(), jane, inner.ID, "Plans (Week 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.RemoveFolder(t.Context(), jane, marketing.ID); err != nil {
		t.Fatalf("remove the top-level folder: %v", err)
	}
	for _, f := range []artifactsdomain.Folder{plans, week} {
		got := n.folder(f.ID)
		if got.Parent != "" || got.Root != f.ID || got.Visibility != artifactsdomain.FolderCompany {
			t.Errorf("%s after its top-level folder's removal is %+v", f.Name, got)
		}
	}
	if got := n.folder(inner.ID); got.Root != week.ID || got.RootOwner != "jane" {
		t.Errorf("Week 1's child is rooted at %q", got.Root)
	}
	if got := n.head(live).Folder; got != "" {
		t.Errorf("the artifact is filed in %q after the top-level removal", got)
	}
	if got := n.count(`SELECT COUNT(*) FROM artifact_folder_shares WHERE folder_id = ?`, marketing.ID); got != 0 {
		t.Errorf("the removed folder keeps %d shares", got)
	}
}

// A FOLDER MOVED TO THE TOP LEVEL KEEPS ITS AUDIENCE, AND ONE MOVED UNDER A
// FOLDER TAKES ITS NEW TREE'S: the move carries its old top-level folder's
// shares, and a top-level folder moved into a tree loses its own and re-roots
// everything beneath it.
func TestAMovedFolderIsSeenByTheTreeItJoins(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	company := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Company", "Team")
	private := n.mkdir(jane, "", artifactsdomain.FolderPrivate, "Mine", "Deep")
	if _, err := n.writer.MoveFolder(t.Context(), jane, company[1].ID, ""); err != nil {
		t.Fatal(err)
	}
	if got := n.folder(company[1].ID); got.Visibility != artifactsdomain.FolderCompany || got.Root != company[1].ID ||
		got.Shares[0].GrantedBy.Name != "jane" {
		t.Errorf("the team folder moved to the top level is %+v", got)
	}
	if _, err := n.writer.MoveFolder(t.Context(), jane, private[0].ID, company[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, f := range private {
		if got := n.folder(f.ID); got.Root != company[0].ID || got.Visibility != artifactsdomain.FolderCompany {
			t.Errorf("%s moved into the company tree is %+v", f.Name, got)
		}
	}
	if got := n.count(`SELECT COUNT(*) FROM artifact_folder_shares WHERE folder_id = ?`, private[0].ID); got != 0 {
		t.Errorf("a folder moved under another keeps %d shares", got)
	}
	if got := n.folder(private[1].ID); got.Depth != 3 {
		t.Errorf("the moved subtree's deepest folder is at depth %d, want 3", got.Depth)
	}
}

// A RECORD INTO A FOLDER REMOVED SINCE ITS DECIDE IS REFUSED WHERE IT
// APPLIES: a create into the folder and a move into it, each on its
// artifact's subject and so not arbitrated against the tree, are refused
// `folder_gone` at their position, on every node alike; the writer refuses
// both from its snapshot first.
//
// Mutation: drop the liveness check in applyCreate and the artifact is filed
// in a tombstone, in no tree any reader sees.
func TestARecordIntoARemovedFolderIsRefusedWhereItApplies(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	folder := artifactsdomain.FolderIDOf("hand-gone", 0).String()
	e.apply(handFolderCreate(t, "hand-gone", "", artifacts.MaxFolderDepth, "Gone"), monday)
	e.apply(handRecord(t, artifactsdomain.FolderSubject(), artifactsdomain.OpRemove, "hand-remove",
		artifactsdomain.ScopeSet{Terms: []artifactsdomain.ScopeTerm{artifactsdomain.TreeTerm(),
			artifactsdomain.ArtifactsTerm()}},
		artifactsdomain.FolderRemovePayload{V: 1, Folder: folder, Authority: suiteAuthority()}), monday)
	created := newID()
	p := createPayload(created)
	p.Folder = folder
	e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: created},
		artifactsdomain.OpCreate, "hand-create-into", artifactsdomain.ScopeSet{Subject: true}, p), monday)
	if outcome, reason, _ := e.verdict(created, "hand-create-into"); outcome != "refused" || reason != "folder_gone" {
		t.Errorf("a create into a removed folder applied as %s %s", outcome, reason)
	}
	if got := e.count(`SELECT COUNT(*) FROM artifacts WHERE id = ?`, created); got != 0 {
		t.Error("the refused create wrote its artifact")
	}
	filed := newID()
	e.apply(handCreate(t, filed, "hand-filed", suiteObject("filed", "<p>f</p>"), limits()), monday)
	e.apply(handRecord(t, artifactsdomain.Subject{Kind: artifactsdomain.KindArtifact, ID: filed},
		artifactsdomain.OpMove, "hand-move-into", artifactsdomain.ScopeSet{Subject: true},
		artifactsdomain.MovePayload{V: 1, To: folder, Authority: suiteAuthority()}), monday)
	if outcome, reason, _ := e.verdict(filed, "hand-move-into"); outcome != "refused" || reason != "folder_gone" {
		t.Errorf("a move into a removed folder applied as %s %s", outcome, reason)
	}

	n := newHarness(t)
	jane := person("jane", artifacts.RoleOwner)
	gone := n.mkdir(jane, "", artifactsdomain.FolderCompany, "Gone")[0]
	id := n.create(jane, "Plan", upload(t, "index.html", "<p>p</p>")).Artifact
	if _, err := n.writer.RemoveFolder(t.Context(), jane, gone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Move(t.Context(), jane, id, gone.ID); !errors.Is(err, artifactsdomain.ErrFolderNotFound) {
		t.Errorf("filing into a removed folder: %v", err)
	}
	if _, err := n.writer.Create(t.Context(), jane, artifactsdomain.CreateInput{
		Kind: artifacts.KindHTML, Title: "Into", Owner: "jane", OwnerKind: iam.ActorHuman, Folder: gone.ID,
		First: page(upload(t, "index.html", "<p>into</p>"))}); !errors.Is(err, artifactsdomain.ErrFolderNotFound) {
		t.Errorf("creating into a removed folder: %v", err)
	}
}

// THE FOLDER HISTORY IS KEPT A YEAR OF BROKER TIME, pruned in the same bounded
// batches as an artifact's.
func TestFolderHistoryIsPrunedAfterAYearOfBrokerTime(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	e.apply(handFolderCreate(t, "hand-old", "", artifacts.MaxFolderDepth, "Old"), monday)
	e.apply(handFolderCreate(t, "hand-fresh", "", artifacts.MaxFolderDepth, "Fresh"),
		monday.Add(artifacts.HistoryRetention+time.Hour))
	old := artifactsdomain.FolderIDOf("hand-old", 0).String()
	if got := e.count(`SELECT COUNT(*) FROM artifact_folder_history WHERE folder_id = ?`, old); got != 0 {
		t.Error("a folder history row older than the retention survived a later record")
	}
	if got := e.count(`SELECT COUNT(*) FROM artifact_folders WHERE id = ?`, old); got != 1 {
		t.Error("pruning the history removed the folder itself")
	}
}

// handFolderCreate is a create of a chain of names under parent, by hand,
// stamped with depth as its bound.
func handFolderCreate(t *testing.T, opID, parent string, depth int, names ...string) []byte {
	t.Helper()
	specs := make([]artifactsdomain.FolderSpec, len(names))
	at := parent
	for i, raw := range names {
		name, key, err := foldername.Check(raw)
		if err != nil {
			t.Fatal(err)
		}
		specs[i] = artifactsdomain.FolderSpec{ID: artifactsdomain.FolderIDOf(opID, i).String(), Parent: at,
			Name: name, NameKey: key}
		at = specs[i].ID
	}
	p := artifactsdomain.FolderCreatePayload{V: 1, Folders: specs, Owner: "jane", OwnerKind: iam.ActorHuman,
		MaxDepth: depth, MaxFolders: artifacts.MaxFolders, MaxChildren: artifacts.MaxFolderChildren,
		Authority: suiteAuthority()}
	if parent == "" {
		p.Shares = []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
			Role: artifacts.RoleContributor, GrantedBy: iam.Actor{Name: "jane", Kind: iam.ActorHuman}}}
	}
	return handRecord(t, artifactsdomain.FolderSubject(), artifactsdomain.OpCreate, opID,
		artifactsdomain.ScopeSet{Subject: true}, p)
}

// folderVerdict is the outcome and reason operation opID's folder history row
// says.
func (e *estate) folderVerdict(opID string) (outcome, reason string) {
	e.t.Helper()
	if err := e.db.Read(e.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(e.t.Context(), `
			SELECT outcome, reason FROM artifact_folder_history WHERE op_id = ?`, opID).Scan(&outcome, &reason)
	}); err != nil {
		e.t.Fatalf("read the folder verdict of %s: %v", opID, err)
	}
	return outcome, reason
}
