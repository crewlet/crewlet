package builtin_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// handsOverWords is EVERY TOOL WHOSE ANSWER HANDS A MODEL WORDS SOMEBODY ELSE
// WROTE, on either surface — the set the sweep below DERIVES — and the case
// that holds what each reads those words as: the surface, the writers and the
// trust on each fence, and the envelope. A case is `<TestName>` or
// `<TestName>/<subtest>`, and must exist.
//
// The sweep decides membership and this table only follows it: a tool the
// sweep finds handing words over that is not here fails until somebody writes
// the case that holds its labels, and a tool here that hands nothing over
// fails until its entry goes. It replaced a hand list in
// TestEveryUntrustedReadIsFenced's doc that named the reads somebody had
// remembered — and the tracker's listings, the inbox, the activity feed, a
// knowledge answer's sources and a parked coding run's question were the ones
// nobody had.
var handsOverWords = map[string]string{
	builtin.AnswerKnowledgeTool:   "TestEveryUntrustedReadIsFenced/answer_knowledge",
	builtin.AnswerRunTool:         "TestEveryUntrustedReadIsFenced/answer_run",
	builtin.CommentOnPageTool:     "TestEveryUntrustedReadIsFenced/comment_on_page",
	worklet.DescribeProjectTool:   "TestEveryUntrustedReadIsFenced/describe_project",
	builtin.GetArtifactTool:       "TestEveryArtifactToolResultIsFenced/get_artifact",
	builtin.GetPageTool:           "TestEveryUntrustedReadIsFenced/get_page",
	builtin.GetWorkItemTool:       "TestEveryUntrustedReadIsFenced/get_work_item",
	builtin.ListArtifactsTool:     "TestEveryArtifactToolResultIsFenced/list_artifacts",
	builtin.ListChannelsTool:      "TestEveryUntrustedReadIsFenced/list_channels",
	builtin.ListPagesTool:         "TestEveryUntrustedReadIsFenced/list_pages",
	worklet.ListProjectFilesTool:  "TestEveryUntrustedReadIsFenced/list_project_files",
	worklet.ListProjectsTool:      "TestEveryUntrustedReadIsFenced/list_projects",
	builtin.ListWorkItemsTool:     "TestEveryUntrustedReadIsFenced/list_work_items",
	worklet.ListWorkViewsTool:     "TestEveryUntrustedReadIsFenced/list_work_views",
	worklet.MyWorkTool:            "TestEveryUntrustedReadIsFenced/my_work",
	builtin.PublishArtifactTool:   "TestEveryArtifactToolResultIsFenced/publish_artifact on a stale base",
	builtin.QueryArtifactDataTool: "TestEveryDataToolResultIsFenced",
	builtin.QueryEpisodesTool:     "TestQueryEpisodesFencesWhatAPastTurnWasAskedAsWhoeverWroteIt",
	builtin.ReadArtifactDataTool:  "TestEveryDataToolResultIsFenced",
	builtin.ReadChannelTool:       "TestEveryUntrustedReadIsFenced/read_channel",
	worklet.ReadProjectFileTool:   "TestEveryUntrustedReadIsFenced/read_project_file",
	builtin.SearchKnowledgeTool:   "TestSearchKnowledgeFencesWhatAPageSays",
	builtin.SearchMessagesTool:    "TestEveryUntrustedReadIsFenced/search_messages",
	builtin.SearchWorkItemsTool:   "TestEveryUntrustedReadIsFenced/search_work_items",
	worklet.TaskActivityTool:      "TestEveryUntrustedReadIsFenced/task_activity",
	chatlet.UpdateChannelTool:     "TestEveryUntrustedReadIsFenced/update_channel",
	worklet.WorkInboxTool:         "TestEveryUntrustedReadIsFenced/work_inbox",
}

// EVERY TOOL THAT HANDS A MODEL SOMEBODY ELSE'S WORDS IS FENCED, AND THE SET
// OF THEM IS DERIVED (ADR-0040).
//
// Every tool this build registers, on a seat's surface and on the operator
// catalogue, is called once against stores where EVERY field a record
// declares somebody's words ([provenance.FieldTag]) — and every body a store
// holds outside a record (a file's bytes, a datastore row, a page a search
// found, a past turn's ask, a parked run's question) — holds words spelling
// the reader's own closer and a forged opener. Each answer is then held to the
// fence's property whatever it carries: a string holding the reader's token is
// one whole fence whose content opens no marker, and no other string opens or
// closes one; and an answer that carries the words carries the envelope (or,
// written as prose, a fence) saying whose they are.
//
// THE SET IS WHAT THE ANSWERS CARRY, never a list somebody keeps: every tool
// whose answer carries the words is in [handsOverWords] beside the case that
// holds its labels, and every tool there carries them. A tool the sweep cannot
// reach — refused for its arguments, unwired — fails rather than drops out of
// the set, so a tool added with no arguments here is a failure naming it, not
// a tool nobody looked at.
//
// Mutation: answer list_work_items through jsonAnswer and its call is refused
// for carrying words it does not label; drop the foreignRead in answer_run and
// its question reaches the operator's model with a forged opener bare; delete
// a handsOverWords entry, or add one, and the table no longer matches.
func TestEveryToolThatHandsOverSomebodysWordsIsFenced(t *testing.T) {
	t.Parallel()
	calls := sweepCalls(t)
	var mu sync.Mutex
	found := map[string][]string{}
	t.Run("each call", func(t *testing.T) {
		for _, c := range calls {
			t.Run(c.name(), func(t *testing.T) {
				t.Parallel()
				r := newSweepRig(t)
				got, err := r.call(t, c)
				if err != nil {
					t.Fatalf("the call ended with %v", err)
				}
				switch {
				case c.refused != "" && (!got.Failed || tools.RefusalOf(got) != c.refused):
					t.Fatalf("answered %v %q, want a %s refusal", tools.RefusalOf(got), got.Output, c.refused)
				case c.refused == "" && got.Failed:
					t.Fatalf("the sweep could not reach the answer, so it cannot say whether the "+
						"tool hands over somebody's words — give the call what it needs in "+
						"sweepArgs or the rig: %s", got.Output)
				}
				if sweepDefused(t, got.Output) {
					mu.Lock()
					found[c.tool] = append(found[c.tool], c.name())
					mu.Unlock()
				}
			})
		}
	})
	if t.Failed() {
		// A call that did not answer leaves the set short: comparing it
		// would report its tool as a stale entry rather than as the call
		// that failed.
		return
	}
	for tool, via := range found {
		if _, held := handsOverWords[tool]; !held {
			t.Errorf("%s hands a model words somebody else wrote (%v) and no case holds what "+
				"it reads them as: write one — the surface, the writers and the trust on "+
				"each fence, and the envelope — and add it to handsOverWords", tool, via)
		}
	}
	cases := caseNames(t)
	for tool, test := range handsOverWords {
		if _, carried := found[tool]; !carried {
			t.Errorf("%s is in handsOverWords, and no call of it hands over anybody's words: "+
				"drop its entry, or give the rig what it would hand over", tool)
		}
		if !cases[test] {
			t.Errorf("%s is held by %s, which is no test or subtest in this package", tool, test)
		}
	}
	// AND THE ONE TEST OF READS HOLDS ONLY READS THAT HAND WORDS OVER, so a
	// case for a tool that hands nothing over is not mistaken for one.
	for name := range cases {
		sub, ok := strings.CutPrefix(name, "TestEveryUntrustedReadIsFenced/")
		if !ok {
			continue
		}
		tool, _, _ := strings.Cut(sub, " ")
		tool, _, _ = strings.Cut(tool, "'")
		if _, carried := found[tool]; !carried {
			t.Errorf("TestEveryUntrustedReadIsFenced has a case %q for %s, whose answer hands "+
				"over nobody's words", sub, tool)
		}
	}
}

// AND THE ARGUMENTS THE SWEEP CALLS WITH ARE ALL FOR TOOLS IT CALLS: an entry
// for a tool gone from both surfaces is an entry nothing reads.
func TestTheSweepsArgumentsAreForRegisteredTools(t *testing.T) {
	t.Parallel()
	r := newSweepRig(t)
	for name := range r.args {
		_, seat := r.seat.Lookup(name)
		_, operator := r.operator.Lookup(name)
		if !seat && !operator {
			t.Errorf("the sweep holds arguments for %s, which neither surface registers", name)
		}
	}
}

// sweepCall is one call the sweep makes: a tool on a surface, with the rig's
// arguments for it, or a variant's — a second answer the same tool gives.
type sweepCall struct {
	surface string
	tool    string
	variant string
	args    map[string]any
	// refused is the refusal the variant is answered with, where the answer
	// that hands words over IS a refusal (a stale publish).
	refused tools.Refusal
}

func (c sweepCall) name() string {
	if c.variant == "" {
		return c.surface + "/" + c.tool
	}
	return c.surface + "/" + c.tool + "/" + c.variant
}

// sweepVariants are the answers a tool gives besides its ordinary one that
// hand words over: a thread rather than a room, and a stale publish's live
// version.
func sweepVariants(r *sweepRig) []sweepCall {
	return []sweepCall{
		{tool: builtin.ReadChannelTool, variant: "thread",
			args: map[string]any{"channel": chatRoomID, "thread": "m1"}},
		{tool: builtin.PublishArtifactTool, variant: "stale base", refused: tools.RefusalStaleVersion,
			args: map[string]any{"artifact": r.artifact, "base_version": 1, "content": "<p>plain</p>"}},
	}
}

// sweepCalls is every registered tool on each surface, and each variant on
// each surface that registers its tool.
func sweepCalls(t *testing.T) []sweepCall {
	t.Helper()
	r := newSweepRig(t)
	var out []sweepCall
	for _, surface := range []string{"seat", "operator"} {
		reg := r.registry(surface)
		for _, name := range reg.Names() {
			out = append(out, sweepCall{surface: surface, tool: name, args: r.args[name]})
		}
		for _, v := range sweepVariants(r) {
			if _, held := reg.Lookup(v.tool); held {
				v.surface = surface
				out = append(out, v)
			}
		}
	}
	if len(out) < 100 {
		t.Fatalf("the sweep has %d calls to make — the rig registered too little to "+
			"certify anything", len(out))
	}
	return out
}

// sweepRig is every tool this build registers, on both surfaces, over stores
// whose every declared field holds the sweep's words.
type sweepRig struct {
	turn     *turnctx.Turn
	seat     *tools.Registry
	operator *tools.Registry
	args     map[string]map[string]any
	artifact string
}

// sweepWords marks the words the rig's stores hold, so an answer that carries
// them is found whatever else it carries.
const sweepWords = "SWEEP-WORDS"

func newSweepRig(t *testing.T) *sweepRig {
	t.Helper()
	turn, b := readerTurn(t)
	words := sweepWords + " " + attack(b, "somebody's words")

	trk := newFakeTracker()
	trk.tasks["ENG-5"] = worklet.TaskDetail{
		Task: worklet.Task{ID: "id-5", Key: "ENG-5", Project: "ENG",
			Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
			Removed: &worklet.Tombstone{By: "ops", At: fakeAt}},
		Complete: true,
	}
	for k, d := range trk.tasks {
		poison(t, reflect.ValueOf(&d).Elem(), words)
		trk.tasks[k] = d
	}
	poison(t, reflect.ValueOf(&trk.activity).Elem(), words)
	poison(t, reflect.ValueOf(&trk.myWork).Elem(), words)
	poison(t, reflect.ValueOf(&trk.projects).Elem(), words)
	poison(t, reflect.ValueOf(&trk.project).Elem(), words)
	poison(t, reflect.ValueOf(&trk.viewListing).Elem(), words)
	poison(t, reflect.ValueOf(&trk.thread).Elem(), words)
	inbox := worklet.InboxAnswer{}
	poison(t, reflect.ValueOf(&inbox).Elem(), words)
	trk.inbox = &inbox
	trk.ranked = []worklet.Ranked{{ID: "i1", Key: "ENG-1"}}
	poison(t, reflect.ValueOf(&trk.ranked).Elem(), words)

	kb := newFakeKB()
	poison(t, reflect.ValueOf(&kb.page).Elem(), words)

	talk := newFakeChat()
	poison(t, reflect.ValueOf(&talk.room).Elem(), words)
	talk.transcript = []chatlet.MessageView{{Message: chatlet.Message{ID: "m1", ChannelID: chatRoomID,
		Author: "jane", AuthorKind: chatlet.AuthorHuman}}}
	poison(t, reflect.ValueOf(&talk.transcript).Elem(), words)
	talk.replies = []chatlet.MessageView{{Message: chatlet.Message{ID: "m2", ChannelID: chatRoomID,
		Author: "agent-ceo", AuthorKind: chatlet.AuthorAgent}}}
	poison(t, reflect.ValueOf(&talk.replies).Elem(), words)
	talk.rootBody = words
	talk.last = &chatlet.MessagePreview{}
	poison(t, reflect.ValueOf(talk.last).Elem(), words)
	talk.hits = []search.ChatHit{{MessageID: "m1", ChannelID: chatRoomID, Snippet: words}}

	files, objects := newFakeFiles(), newFakeObjects()
	obj, err := objects.Put(t.Context(), strings.NewReader(words), 1<<20, objstore.PutMeta{})
	if err != nil {
		t.Fatal(err)
	}
	file := worklet.File{Project: "ENG", Path: "notes.md", ContentType: "text/markdown; charset=utf-8",
		Hash: obj.Hash, Size: obj.Size, Object: obj.Key, Version: 1,
		CreatedBy: "founder", UpdatedBy: "agent-ceo"}
	poison(t, reflect.ValueOf(&file).Elem(), words)
	files.files["ENG/notes.md"] = file

	// AN ARTIFACT AT ITS SECOND VERSION, so a publish naming the first is the
	// stale one that is handed the live text back.
	arts := newFakeArtifacts()
	h := arts.seed("agent-cto", iam.ActorAgent, cto,
		[]artifacts.Share{shareWith("founder", artifacts.RoleEditor),
			{Audience: artifacts.Audience{Kind: artifacts.AudienceCompany}, Role: artifacts.RoleEditor}},
		[2]string{"index.html", "<p>first</p>"})
	arts.republish(h.ID, founder, [2]string{"index.html", words})
	arts.mu.Lock()
	held := arts.heads[h.ID]
	held.Capabilities = []artifacts.Capability{artifacts.CapabilityAssets}
	poison(t, reflect.ValueOf(&held).Elem(), words)
	arts.heads[h.ID] = held
	arts.mu.Unlock()
	// A FOLDER EVERYBODY MAY FILE IN, so filing and making a subfolder reach
	// their answers.
	arts.folder("f-marketing", "", "Marketing", "founder", iam.ActorHuman, companyShare)

	data := newFakeData()
	data.collection("notes", artifacts.ReadAll)
	row, err := json.Marshal(map[string]string{"note": words})
	if err != nil {
		t.Fatal(err)
	}
	data.row("notes", "d1", founder, string(row))

	store := &skillStore{skills: []learning.Skill{{AgentHandle: "agent-cto", Name: "deploys",
		Content: "plain steps"}}}
	// A PAST TURN ASKED BY SOMEBODY, as the turn recorded who: an ask with no
	// label is the company's own instruction, which is read unfenced.
	askedBy := provenance.LabelForHosted("agent-cto", provenance.SourceChatThread,
		[]provenance.Author{{Kind: iam.ActorHuman, Seat: "founder"}}, true)
	episodes := &episodeStore{episodes: []learning.Episode{{Handle: "agent-cto", Kind: learning.KindRaw,
		Ask: words, AskProvenance: &askedBy}}}

	deps := builtin.Deps{
		A2A: &asker{}, Skills: store, Refinable: store,
		Episodes: episodes, Diary: &diaryStore{}, Onboarding: &onboardingStore{},
		ToolSkills: sweepToolSkills{}, Knowledge: sweepSearcher{words: words},
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Search: trk, Inbox: trk,
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
			Merges:        trk.merges, Moves: trk.moves, Dependencies: trk.depends,
			Files: files, FileWriter: files.as, Objects: objects,
		},
		Pages:        builtin.PageDeps{Reader: kb, Writer: kb},
		Artifacts:    artifactDeps(arts, artifactChart{}, requesters{}),
		ArtifactData: artifactDataDeps(data),
		Chat:         chatDeps(talk),
		Authorize:    builtin.Decide(chartLeads),
	}
	seat := tools.NewRegistry()
	if _, err := builtin.Register(seat, deps); err != nil {
		t.Fatal(err)
	}

	// THE OPERATOR CATALOGUE OVER THE SAME STORES, acting as whoever the
	// request's principal is, with every seam it alone has wired.
	company := organization(t)
	w := newLedgerFake(statelog.OutcomeApplied)
	op := operatorDeps(t)
	op.Org = func() *org.Organization { return company }
	op.Work.Reader, op.Work.Writer, op.Work.Search, op.Work.Inbox = trk, trk.as, trk, trk
	op.Work.Merges, op.Work.Moves, op.Work.Dependencies = trk.merges, trk.moves, trk.depends
	op.Work.ProjectWriter = func(builtin.Actor) builtin.ProjectWriter { return trk }
	op.Work.ViewWriter = func(builtin.Actor) builtin.ViewWriter { return w }
	op.Work.CatalogueWriter = func(builtin.Actor) builtin.CatalogueWriter { return w }
	op.Work.TrashWriter = func(builtin.Actor) builtin.TrashWriter { return w }
	op.Work.Placer = func(builtin.Actor) builtin.WorkPlacer { return w }
	op.Work.Files, op.Work.FileWriter, op.Work.Objects = files, files.as, objects
	op.Work.Actor = builtin.PrincipalActor
	op.Pages = deps.Pages
	op.Pages.Actor = builtin.PrincipalPageActor
	op.Knowledge = deps.Knowledge
	op.Artifacts, op.ArtifactData = deps.Artifacts, deps.ArtifactData
	op.Chat = deps.Chat
	op.Chat.Cursors, op.Chat.Actor = fakeCursors{}, builtin.PrincipalChatActor
	op.Pauses.Org = op.Org
	// THE RUN RECORDS the operator's steer_turn reads first, where a note
	// waits once the turn has launched its coding run: "turn-1" has none,
	// so the note goes to the fleet's scatter.
	op.Steer.Runs = &runRecords{}
	op.Runs.Desk = &deskFake{runs: map[string]sandbox.PendingRun{"turn-1": {TurnID: "turn-1",
		Status: sandbox.StatusAwaiting, AgentHandle: "agent-cto", AgentID: ctoSeat(t).String(),
		Requester: "founder", Audience: "founder", Question: words, LaunchID: "launch-turn-1"}}}
	operator := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(op) {
		if err := operator.RegisterWith(tool, tools.OriginBuiltin,
			builtin.AnnotationsFor(tool.Name())); err != nil {
			t.Fatal(err)
		}
	}

	args := sweepArgs(h.ID.String())
	maps.Copy(args, everyWriteCall)
	// NO RENAME, which is the container's lead's: the sweep's chart leads
	// projects and nothing else, and a save is what is being swept.
	args[builtin.SavePageTool] = map[string]any{"page": "p1", "base_version": 4, "body": "plain"}
	return &sweepRig{turn: turn, seat: seat, operator: operator, args: args, artifact: h.ID.String()}
}

func (r *sweepRig) registry(surface string) *tools.Registry {
	if surface == "operator" {
		return r.operator
	}
	return r.seat
}

// call makes one sweep call: on a seat's surface in agent-cto's turn, as its
// own principal, and on the operator catalogue as the founder at the
// dashboard, outside any turn.
func (r *sweepRig) call(t *testing.T, c sweepCall) (tools.Result, error) {
	t.Helper()
	entry, ok := r.registry(c.surface).Lookup(c.tool)
	if !ok {
		t.Fatalf("%s is not registered on the %s surface", c.tool, c.surface)
	}
	if c.surface == "operator" {
		return entry.Tool.Call(withRequestKey(personBoundTo("founder", iam.AllGrants...), request(5)), c.args)
	}
	ctx := turnctx.WithPrincipal(t.Context(), r.turn)
	if tool, ok := entry.Tool.(tools.SeatCallable); ok {
		return tool.CallForTurn(ctx, r.turn, c.args)
	}
	return entry.Tool.Call(ctx, c.args)
}

// sweepArgs is what each tool the write table does not cover is called with:
// plain words, so whatever an answer carries of the sweep's came from a store.
func sweepArgs(artifact string) map[string]map[string]any {
	return map[string]map[string]any{
		builtin.A2AAskTool:               {"target": "agent-ceo", "brief": "a plain brief"},
		builtin.AnswerKnowledgeTool:      {"q": "how do we deploy"},
		builtin.AnswerRunTool:            {"turn_id": "turn-1", "launch_id": "launch-turn-1", "answer": "yes"},
		builtin.ArtifactFormatTool:       {"kind": "doc"},
		builtin.CreateArtifactTool:       {"kind": "html", "title": "Plain title", "content": "<p>plain</p>"},
		builtin.CreateArtifactFolderTool: {"parent": "Marketing", "name": "Plain folder"},
		worklet.DescribeProjectTool:      {"project": "ENG"},
		builtin.GetArtifactTool:          {"artifact": artifact},
		builtin.GetPageTool:              {"page": "p1"},
		worklet.GetPersonTool:            {"handle": "agent-ceo"},
		builtin.GetWorkItemTool:          {"item": "ENG-1"},
		worklet.ListProjectFilesTool:     {"project": "ENG"},
		worklet.ListWorkViewsTool:        {"container": "project:ENG"},
		builtin.LoadToolSkillTool:        {"key": "deploys"},
		builtin.LookupColleagueTool:      {"query": "agent-ceo"},
		builtin.MoveArtifactTool:         {"artifacts": []any{artifact}, "folder": "Marketing"},
		builtin.PauseSeatTool:            {"handle": "agent-ceo"},
		builtin.PublishArtifactTool:      {"artifact": artifact, "base_version": 2, "content": "<p>plain</p>"},
		builtin.QueryArtifactDataTool:    {"artifact": artifact, "collection": "notes"},
		builtin.ReadArtifactDataTool:     {"artifact": artifact, "collection": "notes"},
		builtin.ReadChannelTool:          {"channel": chatRoomID},
		worklet.ReadProjectFileTool:      {"project": "ENG", "path": "notes.md"},
		builtin.RefineSkillTool:          {"skill_name": "deploys", "content": "plain"},
		builtin.ReflectAndPersistTool:    {"content": "a plain fact"},
		worklet.RemoveProjectFileTool:    {"project": "ENG", "path": "notes.md"},
		builtin.ResumeSeatTool:           {"handle": "agent-ceo"},
		builtin.SearchKnowledgeTool:      {"query": "deploy"},
		builtin.SearchMessagesTool:       {"text": "backoff"},
		builtin.SearchWorkItemsTool:      {"text": "checkout"},
		builtin.SetArtifactSchemaTool:    {"artifact": artifact, "collections": []any{map[string]any{"name": "notes"}}},
		builtin.ShareArtifactTool:        {"artifact": artifact, "to": "agent-ceo", "role": "viewer"},
		builtin.SteerTurnTool:            {"turn_id": "turn-1", "note": "plain"},
		builtin.UnshareArtifactTool:      {"artifact": artifact, "from": "founder"},
		builtin.UpdateArtifactTool:       {"artifact": artifact, "icon": "chart"},
		builtin.UploadArtifactAssetTool:  {"artifact": artifact, "name": "logo.png", "content": "iVBORw0KGgo=", "encoding": "base64"},
		builtin.UseSkillTool:             {"skill_name": "deploys"},
		worklet.WorkInboxTool:            {"handle": "agent-ceo"},
		builtin.WriteArtifactDataTool: {"artifact": artifact, "ops": []any{map[string]any{"op": "insert",
			"collection": "notes", "fields": map[string]any{"note": "plain"}}}},
		worklet.WriteProjectFileTool: {"project": "ENG", "path": "plain.md", "content": "plain"},
	}
}

// sweepSearcher finds one page, whatever is asked: its title and its snippet
// are the sweep's words.
type sweepSearcher struct{ words string }

func (sweepSearcher) CanSearch(*org.Role, *org.Organization) bool { return true }

func (s sweepSearcher) Search(context.Context, knowledge.Query) knowledge.Result {
	return knowledge.Result{Hits: []knowledge.Hit{{Title: s.words, Snippet: s.words, PageID: "p1",
		Provenance: provenance.LabelForHosted("", provenance.SourcePageBody, nil, false)}}}
}

// sweepToolSkills holds one tool skill: the company's own instruction to its
// seats, which no fence holds (ADR-0040's "a schedule's task").
type sweepToolSkills struct{}

func (sweepToolSkills) Load(key string) (skills.Loaded, bool) {
	if key != "deploys" {
		return skills.Loaded{}, false
	}
	return skills.Loaded{Body: "# Deploys\n\nRun the checks first.", PageID: "s1", Title: "Deploys"}, true
}

func (sweepToolSkills) Keys() []string { return []string{"deploys"} }

// sweepDefused holds output to the fence's property, whatever token it
// carries, and reports whether it carries the sweep's words: a JSON answer
// through [fencesIn] and the envelope, prose through [proseFencesIn].
func sweepDefused(t *testing.T, output string) bool {
	t.Helper()
	carries := strings.Contains(output, sweepWords)
	var answer map[string]any
	if json.Unmarshal([]byte(output), &answer) == nil {
		fencesIn(t, tokenIn(output), output)
		if _, labelled := answer["provenance"]; carries && !labelled {
			t.Errorf("the answer hands over somebody's words with no envelope saying whose:\n%s", output)
		}
		return carries
	}
	fences := proseFencesIn(t, tokenIn(output), output)
	if carries && fences == 0 {
		t.Errorf("the answer hands over somebody's words with no fence saying whose:\n%s", output)
	}
	return carries
}

// proseFencesIn is [fencesIn] for an answer written as prose: every marker in
// it is an opener or a closer of b, as many of each, and with those removed
// nothing left carries b or opens a marker. It answers the number of fences.
func proseFencesIn(t *testing.T, b provenance.Boundary, output string) int {
	t.Helper()
	openers := regexp.MustCompile(`\[untrusted:` + string(b) + ` [^\]\n]*\]`)
	closers := regexp.MustCompile(`\[/untrusted:` + string(b) + `\]`)
	opened, closed := len(openers.FindAllString(output, -1)), len(closers.FindAllString(output, -1))
	if opened != closed {
		t.Errorf("the answer opens %d fences and closes %d:\n%s", opened, closed, output)
	}
	rest := closers.ReplaceAllString(openers.ReplaceAllString(output, ""), "")
	if strings.Contains(strings.ToUpper(rest), string(b)) {
		t.Errorf("the answer carries its token outside a fence line:\n%s", output)
	}
	if markerShaped.MatchString(rest) {
		t.Errorf("the answer opens a marker outside a fence line:\n%s", output)
	}
	return opened
}

// poison fills every field v declares somebody's words ([provenance.FieldTag])
// with words — giving a list, a map or a pointer on the way to one an entry
// to hold them — so whatever a tool hands over of a record, the words are in
// it. v must be settable.
func poison(t *testing.T, v reflect.Value, words string) {
	t.Helper()
	poisonAt(t, v, words, 0)
}

// poisonDepth bounds the walk, which follows a record's own types: a record
// is a few levels deep, and a type that nests itself is filled this far.
const poisonDepth = 12

func poisonAt(t *testing.T, v reflect.Value, words string, depth int) {
	t.Helper()
	if depth > poisonDepth {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			if !declares(v.Type().Elem(), map[reflect.Type]bool{}) {
				return
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		poisonAt(t, v.Elem(), words, depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if _, declared := f.Tag.Lookup(provenance.FieldTag); declared {
				fill(t, v.Field(i), words, f)
				continue
			}
			if declares(f.Type, map[reflect.Type]bool{}) {
				poisonAt(t, v.Field(i), words, depth+1)
			}
		}
	case reflect.Slice:
		if v.Len() == 0 {
			v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
		}
		for i := 0; i < v.Len(); i++ {
			poisonAt(t, v.Index(i), words, depth+1)
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		if v.Len() == 0 {
			v.SetMapIndex(reflect.ValueOf("sweep").Convert(v.Type().Key()), reflect.Zero(v.Type().Elem()))
		}
		for _, k := range v.MapKeys() {
			entry := reflect.New(v.Type().Elem()).Elem()
			entry.Set(v.MapIndex(k))
			poisonAt(t, entry, words, depth+1)
			v.SetMapIndex(k, entry)
		}
	}
}

// fill sets one declared field to words, in whatever shape it holds text.
func fill(t *testing.T, v reflect.Value, words string, f reflect.StructField) {
	t.Helper()
	encoded, err := json.Marshal(words)
	if err != nil {
		t.Fatal(err)
	}
	isRaw := func(t reflect.Type) bool {
		return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
	}
	switch typ := v.Type(); {
	case typ.Kind() == reflect.String:
		v.SetString(words)
	case isRaw(typ):
		v.SetBytes(encoded)
	case typ.Kind() == reflect.Slice && typ.Elem().Kind() == reflect.String:
		v.Set(reflect.ValueOf([]string{words}).Convert(typ))
	case typ.Kind() == reflect.Map && typ.Key().Kind() == reflect.String:
		var entry reflect.Value
		switch elem := typ.Elem(); {
		case elem.Kind() == reflect.String:
			entry = reflect.ValueOf(words).Convert(elem)
		case elem.Kind() == reflect.Interface:
			entry = reflect.ValueOf(words)
		case isRaw(elem):
			entry = reflect.ValueOf(encoded).Convert(elem)
		default:
			t.Fatalf("%s is declared %s and holds %s, whose entries hold no text", f.Name,
				provenance.FieldTag, typ)
		}
		m := reflect.MakeMap(typ)
		m.SetMapIndex(reflect.ValueOf("sweep").Convert(typ.Key()), entry)
		v.Set(m)
	default:
		t.Fatalf("%s is declared %s and holds %s, which holds no text", f.Name,
			provenance.FieldTag, typ)
	}
}

// declares reports whether a value of typ can hold a declared field.
func declares(typ reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[typ] {
		return false
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return declares(typ.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if _, declared := f.Tag.Lookup(provenance.FieldTag); declared || declares(f.Type, seen) {
				return true
			}
		}
	}
	return false
}

// caseNames is every test in this package and every subtest one names with a
// string literal, as `<TestName>` and `<TestName>/<subtest>`.
func caseNames(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			out[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Run" {
					return true
				}
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if name, err := strconv.Unquote(lit.Value); err == nil {
						out[fn.Name.Name+"/"+name] = true
					}
				}
				return true
			})
		}
	}
	if len(out) < 100 || !slices.ContainsFunc(slices.Collect(maps.Keys(out)), func(n string) bool {
		return strings.HasPrefix(n, "TestEveryUntrustedReadIsFenced/")
	}) {
		t.Fatalf("read %d tests and subtests — the walk certified nothing", len(out))
	}
	return out
}
