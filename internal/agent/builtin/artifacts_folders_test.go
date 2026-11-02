package builtin_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// companyShare is a top-level folder's "Everyone at the company" preset.
var companyShare = artifacts.Share{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany},
	Role: artifacts.RoleContributor}

// strangerTurn is agent-cto's turn woken by somebody the company does not
// know.
func strangerTurn(t *testing.T) *turnctx.Turn {
	t.Helper()
	turn := turnFor(t, "agent-cto")
	turn.Senders = []types.CanonicalIdentity{{}}
	return turn
}

// folderRig is the artifacts tools over a tree: Marketing (everyone, made by
// the founder) holding Campaigns, the founder's private Board, and a folder the
// founder shared with agent-cto as a viewer only.
func folderRig(t *testing.T) (artifactRig, map[string]artifactsdomain.Folder) {
	t.Helper()
	req := requesters{"founder": founderPrincipal(iam.Apps...)}
	r := newArtifactRig(t, artifactChart{}, req, nil)
	fs := map[string]artifactsdomain.Folder{}
	fs["marketing"] = r.f.folder("f-marketing", "", "Marketing", "founder", iam.ActorHuman, companyShare)
	fs["campaigns"] = r.f.folder("f-campaigns", "f-marketing", "Campaigns", "founder", iam.ActorHuman)
	fs["board"] = r.f.folder("f-board", "", "Board", "founder", iam.ActorHuman)
	fs["readonly"] = r.f.folder("f-readonly", "", "Finance", "founder", iam.ActorHuman,
		shareWith("agent-cto", artifacts.RoleViewer))
	return r, fs
}

// A FOLDER PATH NAMING SEVERAL FOLDERS IS REFUSED WITH EACH CANDIDATE, and
// nothing is created: a company "Drafts" and the agent's own private "Drafts"
// share a name at the top level, so the agent is told both ids, owners and
// audiences and passes the one it means. The argument to change, `invalid`:
// the same path never lands, an id does.
func TestAFolderPathNamingSeveralFoldersIsRefusedWithItsCandidates(t *testing.T) {
	t.Parallel()
	r, _ := folderRig(t)
	company := r.f.folder("f-drafts-company", "", "Drafts", "founder", iam.ActorHuman, companyShare)
	own := r.f.folder("f-drafts-own", "", "drafts", "agent-cto", iam.ActorAgent)
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "Drafts", "name": "Q3"})
	if tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Fatalf("an ambiguous parent answered %v: %s", tools.RefusalOf(got), got.Output)
	}
	for _, want := range []string{company.ID, own.ID, "everyone at the company", "only its owner"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, got.Output)
		}
	}
	var ambiguous *artifactsdomain.FolderAmbiguous
	if !errors.As(got.Cause, &ambiguous) {
		t.Errorf("the cause is not the domain's ambiguity: %v", got.Cause)
	}
	if len(r.f.folderCreates) != 0 {
		t.Fatalf("an ambiguous parent created folders: %+v", r.f.folderCreates)
	}
	// BY ITS ID, the one meant is unambiguous.
	got = r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactFolderTool,
		map[string]any{"parent": own.ID, "name": "Q3"})
	if got.Failed {
		t.Fatalf("the parent by id: %s", got.Output)
	}
}

// A STRANGER'S TURN SEES NO FOLDER AND FILES IN NONE: it creates no folder,
// moves nothing into one, lists no prefix and is shown no folder's name,
// though the company's own tree is visible to everyone at the company. The
// brand, the company's public face, stays readable (see the brand tests).
func TestAStrangersTurnSeesNoFolderAndFilesInNone(t *testing.T) {
	t.Parallel()
	r, fs := folderRig(t)
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})
	r.f.mu.Lock()
	held := r.f.heads[h.ID]
	held.Folder = fs["campaigns"].ID
	r.f.heads[h.ID] = held
	r.f.prefixes = []artifactsdomain.CommonPrefix{{Prefix: "Marketing/", Folder: fs["marketing"].ID,
		Name: "Marketing"}}
	r.f.mu.Unlock()

	for name, args := range map[string]map[string]any{
		builtin.CreateArtifactFolderTool: {"parent": "Marketing", "name": "Leak"},
		builtin.MoveArtifactTool:         {"artifacts": []any{h.ID.String()}, "folder": "Marketing"},
		builtin.ListArtifactsTool:        {"prefix": "Marketing/"},
	} {
		got := r.call(t, strangerTurn(t), name, args)
		if tools.RefusalOf(got) != tools.RefusalForbidden || strings.Contains(got.Output, "Campaigns") {
			t.Errorf("%s under a stranger answered %v: %s", name, tools.RefusalOf(got), got.Output)
		}
	}
	got := r.call(t, strangerTurn(t), builtin.ListArtifactsTool, map[string]any{"delimiter": "/"})
	if got.Failed || strings.Contains(got.Output, "Marketing") || strings.Contains(got.Output, "Campaigns") {
		t.Fatalf("a stranger's one-level listing disclosed a folder: %s", got.Output)
	}
	if len(r.f.folderCreates) != 0 || len(r.f.moves) != 0 {
		t.Fatalf("a stranger's turn wrote: creates %v, moves %v", r.f.folderCreates, r.f.moves)
	}
}

// FILING ASKS THE FOLDER AND THE ARTIFACT: an agent that may edit its own
// artifact still files only where it holds contributor (a folder shared with
// it as a viewer is refused, naming the role), and only where the person it
// works for could file too (the requester ceiling).
func TestFilingAsksContributorOnTheFolderForBothPrincipals(t *testing.T) {
	t.Parallel()
	r, fs := folderRig(t)
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "<p>x</p>"})

	got := r.call(t, turnFor(t, "agent-cto"), builtin.MoveArtifactTool,
		map[string]any{"artifacts": []any{h.ID.String()}, "folder": fs["readonly"].ID})
	if tools.RefusalOf(got) != tools.RefusalForbidden || !strings.Contains(got.Output, "contributor") {
		t.Fatalf("filing into a viewer-only folder answered %v: %s", tools.RefusalOf(got), got.Output)
	}
	got = r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "Finance", "name": "Claims"})
	if tools.RefusalOf(got) != tools.RefusalForbidden {
		t.Fatalf("a subfolder of a viewer-only folder answered %v: %s", tools.RefusalOf(got), got.Output)
	}

	// THE AGENT'S OWN PRIVATE FOLDER, which the founder cannot see: the
	// founder's request cannot even name it, and a turn nobody woke files
	// there freely.
	mine := r.f.folder("f-mine", "", "Scratch", "agent-cto", iam.ActorAgent)
	got = r.call(t, asked(t), builtin.MoveArtifactTool,
		map[string]any{"artifacts": []any{h.ID.String()}, "folder": "Scratch"})
	if tools.RefusalOf(got) != tools.RefusalNotFound || strings.Contains(got.Output, mine.ID) {
		t.Fatalf("a folder the requester cannot see answered %v: %s", tools.RefusalOf(got), got.Output)
	}
	got = r.call(t, turnFor(t, "agent-cto"), builtin.MoveArtifactTool,
		map[string]any{"artifacts": []any{h.ID.String()}, "folder": "Scratch"})
	if got.Failed {
		t.Fatalf("filing the agent's own work in its own folder: %s", got.Output)
	}
	if len(r.f.moves) != 1 || r.f.moves[0] != [2]string{h.ID.String(), mine.ID} {
		t.Fatalf("moves %v", r.f.moves)
	}
}

// A MISSING FOLDER IS REFUSED WITH THE NEAREST PATHS THE CALL MAY NAME, and is
// never created: a typo would otherwise mint a folder.
func TestAMissingFolderIsRefusedWithTheNearestPaths(t *testing.T) {
	t.Parallel()
	r, _ := folderRig(t)
	got := r.call(t, asked(t), builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "Marketng/Campaign", "name": "Q3"})
	if tools.RefusalOf(got) != tools.RefusalNotFound {
		t.Fatalf("a missing parent answered %v: %s", tools.RefusalOf(got), got.Output)
	}
	if !strings.Contains(got.Output, "Marketing/Campaigns") {
		t.Errorf("the refusal does not name the nearest path:\n%s", got.Output)
	}
	// THE FOUNDER'S PRIVATE BOARD is the founder's to see, and the agent's
	// own view never includes it, so it is never suggested.
	if strings.Contains(got.Output, "Board") {
		t.Errorf("the refusal names a folder the agent cannot see:\n%s", got.Output)
	}
	if len(r.f.folderCreates) != 0 {
		t.Fatalf("a missing parent created folders: %+v", r.f.folderCreates)
	}
}

// loggedCall runs one tool in turn and records it in the turn's call log
// afterwards, as the engine's tool loop does, so each distinct call derives
// its own operation key.
func (r artifactRig) loggedCall(t *testing.T, turn *turnctx.Turn, name string, args map[string]any) tools.Result {
	t.Helper()
	got := r.call(t, turn, name, args)
	turn.CallLog().Record(name, toolargs.Of(nil, args))
	return got
}

// A FOLDER CHAIN IS MADE ONCE AND ANSWERED AFTER, AND A TURN MAKES FEW: the
// same path again answers the folder it made without charging the turn, and a
// turn segment's sixth new folder record is refused as its quota.
func TestCreatingAFolderIsIdempotentAndBoundedPerTurn(t *testing.T) {
	t.Parallel()
	r, fs := folderRig(t)
	turn := turnFor(t, "agent-cto")
	turn.Calls = turnctx.NewCallLog()
	got := r.loggedCall(t, turn, builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "Marketing", "name": "Campaigns/Q3"})
	out := decode(t, got)
	if got.Failed || out["outcome"] != "created" || out["path"] != "Marketing/Campaigns/Q3" ||
		out["created"] != float64(1) {
		t.Fatalf("a chain under an existing parent answered %s", got.Output)
	}
	if in := r.f.folderCreates[0]; in.Parent != fs["marketing"].ID || !in.Parents ||
		strings.Join(in.Names, "|") != "Campaigns|Q3" || in.Owner != "agent-cto" ||
		in.OwnerKind != iam.ActorAgent {
		t.Errorf("the writer was asked %+v", in)
	}
	again := r.loggedCall(t, turn, builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "marketing", "name": "campaigns/q3"})
	if out := decode(t, again); again.Failed || out["outcome"] != "exists" || out["folder"] == nil {
		t.Fatalf("the same chain again answered %s", again.Output)
	}

	// FOUR MORE NEW ONES fill the segment's five; the sixth is refused, and
	// re-asking for one that exists still answers.
	for i, name := range []string{"A", "B", "C", "D"} {
		got = r.loggedCall(t, turn, builtin.CreateArtifactFolderTool,
			map[string]any{"parent": "Marketing", "name": name})
		if got.Failed {
			t.Fatalf("new folder %d: %s", i+2, got.Output)
		}
	}
	got = r.loggedCall(t, turn, builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "Marketing", "name": "E"})
	if tools.RefusalOf(got) != tools.RefusalForbidden || !strings.Contains(got.Output, "turn_quota") {
		t.Fatalf("the sixth new folder answered %v: %s", tools.RefusalOf(got), got.Output)
	}
	got = r.loggedCall(t, turn, builtin.CreateArtifactFolderTool,
		map[string]any{"parent": "Marketing", "name": "A"})
	if got.Failed {
		t.Fatalf("an existing folder past the quota: %s", got.Output)
	}

	// ONLY A PERSON MAKES A TOP-LEVEL FOLDER, and a path too deep is refused
	// before the writer is asked.
	for name, args := range map[string]map[string]any{
		"no parent":  {"name": "Top"},
		"too deep":   {"parent": "Marketing", "name": "A/B/C/D"},
		"empty name": {"parent": "Marketing", "name": "A//B"},
	} {
		if got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactFolderTool, args); tools.RefusalOf(got) != tools.RefusalInvalid {
			t.Errorf("%s answered %v: %s", name, tools.RefusalOf(got), got.Output)
		}
	}
}

// EACH MOVE IS ITS OWN DECISION AND ITS OWN RECORD: in one call, the agent's
// own artifact is filed, one already there is left as it is, a colleague's
// artifact the agent may not edit is refused (in the words an unknown id is),
// and only the one that changed is written.
func TestMovingArtifactsDecidesAndFilesEachOnItsOwn(t *testing.T) {
	t.Parallel()
	r, fs := folderRig(t)
	mine := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "a"})
	there := r.f.seed("agent-cto", iam.ActorAgent, cto, nil, [2]string{"index.html", "b"})
	r.f.mu.Lock()
	held := r.f.heads[there.ID]
	held.Folder = fs["campaigns"].ID
	r.f.heads[there.ID] = held
	r.f.mu.Unlock()
	theirs := r.f.seed("founder", iam.ActorHuman, founder, nil, [2]string{"index.html", "c"})

	got := r.call(t, turnFor(t, "agent-cto"), builtin.MoveArtifactTool, map[string]any{
		"artifacts": []any{mine.ID.String(), there.ID.String(), theirs.ID.String(), mine.ID.String()},
		"folder":    "Marketing/Campaigns"})
	if got.Failed {
		t.Fatalf("a move with one landing answered failed: %s", got.Output)
	}
	out := decode(t, got)
	results, _ := out["results"].([]any)
	if len(results) != 3 || out["path"] != "Marketing/Campaigns" {
		t.Fatalf("results %v (path %v), want one per distinct artifact", results, out["path"])
	}
	want := []string{"moved", "unchanged", "refused"}
	for i, res := range results {
		if o := res.(map[string]any)["outcome"]; o != want[i] {
			t.Errorf("artifact %d answered %v, want %s: %v", i, o, want[i], res)
		}
	}
	if msg, _ := results[2].(map[string]any)["message"].(string); !strings.Contains(msg, "no artifact") {
		t.Errorf("a colleague's private artifact was refused as %q", msg)
	}
	if len(r.f.moves) != 1 || r.f.moves[0] != [2]string{mine.ID.String(), fs["campaigns"].ID} {
		t.Fatalf("moves %v, want the agent's own alone", r.f.moves)
	}

	// TO THE TOP LEVEL needs no folder at all; every item refused is a
	// failed call.
	got = r.call(t, turnFor(t, "agent-cto"), builtin.MoveArtifactTool,
		map[string]any{"artifacts": []any{mine.ID.String()}, "folder": ""})
	if got.Failed || r.f.moves[len(r.f.moves)-1] != [2]string{mine.ID.String(), ""} {
		t.Fatalf("a move to the top level: %s (moves %v)", got.Output, r.f.moves)
	}
	got = r.call(t, turnFor(t, "agent-cto"), builtin.MoveArtifactTool,
		map[string]any{"artifacts": []any{theirs.ID.String()}, "folder": ""})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalNotFound {
		t.Fatalf("a call whose every item was refused answered %v: %s", tools.RefusalOf(got), got.Output)
	}
}

// THE LISTING SPEAKS S3 AND NAMES ONLY WHAT BOTH MAY SEE: prefix and delimiter
// reach the domain with the requester beside the agent, the answer carries
// contents, common prefixes and the key count, every row names its folder,
// and an invisible prefix is a refusal, never an empty page.
func TestTheListingTakesPrefixesAndAnswersCommonPrefixes(t *testing.T) {
	t.Parallel()
	r, fs := folderRig(t)
	h := r.f.seed("agent-cto", iam.ActorAgent, cto, []artifacts.Share{shareWith("founder", artifacts.RoleViewer)},
		[2]string{"index.html", "a"})
	r.f.mu.Lock()
	held := r.f.heads[h.ID]
	held.Folder = fs["campaigns"].ID
	r.f.heads[h.ID] = held
	r.f.prefixes = []artifactsdomain.CommonPrefix{{Prefix: "Marketing/Campaigns/",
		Folder: fs["campaigns"].ID, Name: "Campaigns", Visibility: artifactsdomain.FolderCompany, Items: 1}}
	r.f.mu.Unlock()

	got := r.call(t, asked(t), builtin.ListArtifactsTool,
		map[string]any{"prefix": "Marketing", "delimiter": "/", "max_keys": 10})
	if got.Failed {
		t.Fatalf("list: %s", got.Output)
	}
	q := r.f.queries[len(r.f.queries)-1]
	if q.Prefix != "Marketing/" || q.Delimiter != "/" || q.MaxKeys != 10 || q.Requester == nil ||
		q.Requester.Record != "founder" {
		t.Fatalf("the domain was asked %+v", q)
	}
	out := decode(t, got)
	contents, _ := out["contents"].([]any)
	prefixes, _ := out["common_prefixes"].([]any)
	if len(contents) != 1 || len(prefixes) != 1 || out["key_count"] != float64(2) {
		t.Fatalf("the answer %s", got.Output)
	}
	// THE HTTP LISTING'S SHAPE: a row's folder by id and by path, and a
	// common prefix's count under `items`.
	if row := contents[0].(map[string]any); row["folder_path"] != "Marketing/Campaigns/" ||
		row["folder"] != fs["campaigns"].ID {
		t.Errorf("the row names folder %v at %v", row["folder"], row["folder_path"])
	}
	if p := prefixes[0].(map[string]any); p["prefix"] != "Marketing/Campaigns/" || p["folder"] != fs["campaigns"].ID ||
		p["items"] == nil || p["artifacts"] != nil {
		t.Errorf("the common prefix %v", p)
	}

	got = r.call(t, asked(t), builtin.ListArtifactsTool, map[string]any{"prefix": "Board/"})
	if tools.RefusalOf(got) != tools.RefusalNotFound {
		t.Fatalf("a prefix the agent cannot see answered %v: %s", tools.RefusalOf(got), got.Output)
	}
	if got := r.call(t, asked(t), builtin.ListArtifactsTool, map[string]any{"delimiter": ","}); tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Errorf("a delimiter other than / answered %v", tools.RefusalOf(got))
	}
}

// A NEW ARTIFACT IS FILED WHERE THE CALL NAMES, under both decisions: the
// resolved folder's id reaches the pipeline, a folder the agent may only view
// is refused, and a missing one is never created.
func TestCreatingAnArtifactFilesItInTheNamedFolder(t *testing.T) {
	t.Parallel()
	r, fs := folderRig(t)
	args := func(folder string) map[string]any {
		return map[string]any{"kind": "html", "title": "Launch plan", "content": "<p>x</p>",
			"folder": folder}
	}
	got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, args("Marketing/Campaigns"))
	if got.Failed {
		t.Fatalf("create into a folder: %s", got.Output)
	}
	if req := r.f.creates[len(r.f.creates)-1]; req.Folder != fs["campaigns"].ID {
		t.Fatalf("the pipeline was asked to file in %q", req.Folder)
	}
	if out := decode(t, got); out["folder"] != "Marketing/Campaigns" {
		t.Errorf("the answer names folder %v", out["folder"])
	}
	for name, folder := range map[string]string{"viewer only": "Finance", "missing": "Marketing/Nope"} {
		before := len(r.f.creates)
		got := r.call(t, turnFor(t, "agent-cto"), builtin.CreateArtifactTool, args(folder))
		if !got.Failed || len(r.f.creates) != before {
			t.Errorf("%s: answered %v and asked the pipeline %d times", name, got.Output,
				len(r.f.creates)-before)
		}
	}
}
