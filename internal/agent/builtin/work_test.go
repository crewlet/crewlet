package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the fakes -------------------------------------------------------- //

type fakeTracker struct {
	// answer overrides what Tasks returns, for the cases that are about
	// the SHAPE of an answer rather than about the rows in it.
	answer *worklet.Answer

	// query is the last one the list tool built, so a case can assert
	// what a model's ARGUMENTS became.
	query worklet.Query

	// params is what the tool handed the grammar, before it was parsed.
	params map[string]string

	// views are the saved views [fakeTracker.ExpandedQuery] expands, by id
	// — a view's own keys under the caller's, as the real expansion loads
	// them.
	views map[string]map[string]any
	tasks map[string]worklet.TaskDetail

	// reads is every freshness the point readers were handed.
	reads []statelog.Freshness

	// wants is the last DetailWants a detail read was given, so a case can
	// assert what a model's arguments BECAME.
	wants worklet.DetailWants

	created []worklet.Task
	asks    []worklet.Comment
	merged  []mergeCall
	moved   []moveCall

	// createAnswer overrides what CreateTask answers, for the cases about
	// what the TOOL makes of an outcome rather than about the task it
	// composed.
	createAnswer *worklet.WriteResult

	// defaultAssignee is who a create naming nobody is reported filed to,
	// standing in for the project's default the real tracker settles on
	// inside the create's own snapshot.
	defaultAssignee string

	// searched is every text the ranked search was asked for, and ranked
	// what it answers with.
	searched  []string
	ranked    []worklet.Ranked
	searchErr error
	// searchModes is every mode a search asked for, partialSearch makes
	// the fake answer over part of the corpus, and searchDegraded makes it
	// answer as a company with nothing to rank by meaning does: a hybrid
	// search served by the words, a semantic one served nothing.
	searchModes    []knowledge.Mode
	partialSearch  bool
	searchDegraded knowledge.Degradation
	patched        []worklet.TaskPatch
	ifMatch        []uint64
	notified       []*worklet.Notify

	// kinds is what each write said it WAS, which is a different fact
	// from whether it notified anybody (see [worklet.MutationRecord.Kind]).
	kinds  []worklet.ChangeKind
	actors []builtin.Actor
	opIDs  []string

	// thread is what Thread answers and threadQuery the last one asked,
	// which is how a case asserts what the comment tool RESOLVED rather
	// than only what it wrote.
	thread      worklet.ResolvedThread
	threadQuery worklet.ThreadQuery
	threadErr   error

	// posted is the remark UpdateTask answers that it found already on the
	// task — the call that posted it, made again — for the cases about what
	// the comment tool reports of a write answered from a row.
	posted *worklet.PostedRemark

	// depended is every dependency change the tool composed, dependErr
	// what the sequence answers, and dependAnswer — when set — the result
	// it answers in place of the default applied commit.
	depended     []worklet.DependencyChange
	dependErr    error
	dependAnswer *worklet.DependencyResult

	projectEdits     []worklet.ProjectEdit
	projectAuthority []worklet.ProjectAuthority
	// projectWarnings is what a policy write reports it was not refused
	// for — a target date given as an instant, stored as its day.
	projectWarnings []caveat.Caveat

	// declaredTypes and declaredFields are the catalogue as the WRITE
	// tools composed it, the half of those verbs this package owns; the
	// tracker's own suite certifies what it does with a declaration, and
	// only this one what a model's arguments became.
	declaredTypes  [][]worklet.TaskType
	declaredFields [][]worklet.FieldDef
	tagEdits       []worklet.TagEdit
	tagAuthority   []worklet.TagAuthority
	tagWarnings    []caveat.Caveat
	ensured        [][]string
	ensuredIn      []string
	// ensureErr is what the INLINE declaration alone answers, so a case can
	// stop a create or an update at its labels while every write after it
	// would succeed — and so catch a tool that carried on to one.
	ensureErr error

	projectQuery worklet.ProjectQuery
	projects     worklet.ProjectListing
	detailQuery  worklet.ProjectDetailQuery
	project      worklet.ProjectDetail

	activityQuery worklet.ActivityQuery
	activity      worklet.ActivityAnswer
	myWorkQuery   worklet.MyWorkQuery
	myWork        worklet.MyWork

	// viewQuery is the last strip the view listing asked for, so a case
	// can assert WHOSE strip a caller was shown.
	viewQuery worklet.ViewQuery
	// viewListing is the strip Views answers.
	viewListing worklet.ViewListing
	// myWorkZone is the clock the last day was cut on, which is the half
	// of that read a tool decides rather than the tracker.
	myWorkZone *time.Location

	// viewer is who the last list was expanded FOR, which is the half of a
	// personal filter — `preset=my_queue`, `assignee=me` — that decides
	// whose queue a caller is answered about.
	viewer worklet.Viewer

	// personQuery and inboxQuery are the last personal reads this fake was
	// asked, so a case can assert WHOSE record a tool resolved the question
	// to.
	personQuery worklet.PersonQuery
	inboxQuery  worklet.InboxQuery
	// inbox is the page Inbox answers, where a case sets one; nil answers
	// its one placeholder notice.
	inbox *worklet.InboxAnswer

	readErr  error
	writeErr error
}

func newFakeTracker() *fakeTracker {
	tasks := map[string]worklet.TaskDetail{
		"ENG-1": {
			Task: worklet.Task{
				ID: "i1", Key: "ENG-1", Project: "ENG", Title: "the work",
				Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
				Version: 7,
			},
			Complete: true,
		},
	}
	// THREE MORE TO POINT AT, because every relation argument resolves its
	// references to IDS before anything is written — a key stored in a
	// relation resolves to nothing on every node, for ever.
	for _, n := range []string{"2", "3", "4"} {
		tasks["ENG-"+n] = worklet.TaskDetail{
			Task: worklet.Task{
				ID: "id-" + n, Key: "ENG-" + n, Project: "ENG",
				Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
			},
			Complete: true,
		}
	}
	return &fakeTracker{tasks: tasks}
}

func (f *fakeTracker) Tasks(_ context.Context, q worklet.Query, _ time.Time) (worklet.Answer, error) {
	f.query = q
	if f.readErr != nil {
		return worklet.Answer{}, f.readErr
	}
	if f.answer != nil {
		return *f.answer, nil
	}
	answer := worklet.Answer{Complete: true, Level: statelog.ReadSession}
	for _, d := range f.tasks {
		row := worklet.TaskRow{
			ID: d.Task.ID, Key: d.Task.Key, Title: d.Task.Title,
			Status: d.Task.Status, Project: d.Task.Project,
		}
		// THE OPT-IN FACTS, as the real reader fills them: present when
		// the query asked, so a surface that forwarded `fields=` shows up
		// in what it renders rather than only in the query it built.
		if q.Wants(worklet.RowFieldSpend) {
			row.Spend = &worklet.RowSpend{Tokens: 1400, Turns: 2}
		}
		if q.Wants(worklet.RowFieldTags) {
			row.Tags = []string{"api"}
		}
		answer.Rows = append(answer.Rows, row)
	}
	slices.SortFunc(answer.Rows, func(a, b worklet.TaskRow) int {
		return strings.Compare(a.ID, b.ID)
	})
	answer.TotalHint = len(answer.Rows)
	return answer, nil
}

func (f *fakeTracker) Task(_ context.Context, idOrKey string, want worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {

	f.reads = append(f.reads, fresh)
	f.wants = want
	if f.readErr != nil {
		return worklet.TaskDetail{}, f.readErr
	}
	for key, d := range f.tasks {
		if key == strings.ToUpper(idOrKey) || d.Task.ID == idOrKey {
			return d, nil
		}
	}
	return worklet.TaskDetail{}, fmt.Errorf("%w: %s", worklet.ErrNoTask, idOrKey)
}

func (f *fakeTracker) CarriedViews(_ context.Context, q worklet.ViewQuery) (worklet.ViewListing, error) {
	f.viewQuery = q
	return f.viewListing, nil
}

func (f *fakeTracker) ExpandedQuery(_ context.Context, params map[string]any,
	viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error) {

	f.viewer = viewer

	// THE KEYS THE TOOL COMPOSED, kept as strings: what a case here is
	// about is the TRANSLATION from a model's arguments to the grammar's
	// own keys, and the parse below is the real one, so a key this tool
	// never forwarded reaches neither.
	f.params = map[string]string{}
	for key, value := range params {
		f.params[key] = fmt.Sprint(value)
	}
	merged := worklet.MapParams{}
	if view, held := f.views[fmt.Sprint(params["view"])]; held {
		for key, value := range view {
			merged[key] = value
		}
	}
	for key, value := range params {
		merged[key] = value
	}
	return worklet.ParseQuery(merged, now, loc)
}

func (f *fakeTracker) CarriedCatalogue(context.Context, worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return worklet.CatalogueAnswer{}, nil
}

// Person answers an empty record under the handle it was ASKED for, so a case
// can assert whose record a name resolved to rather than only that it read one.
func (f *fakeTracker) Person(_ context.Context, q worklet.PersonQuery,
	_ time.Time) (worklet.PersonState, error) {

	f.personQuery = q
	return worklet.PersonState{Handle: q.Handle}, nil
}

// Thread answers what the fake was TOLD to answer, and records the query.
// It is a real seam, not a zero value: the comment tool's ask handling turns
// on who is in the thread and which open ask was inferred, so a stub
// returning nothing would let every one of those arms pass doing nothing.
func (f *fakeTracker) Thread(_ context.Context, q worklet.ThreadQuery,
	fresh statelog.Freshness) (worklet.ResolvedThread, error) {

	f.reads = append(f.reads, fresh)

	f.threadQuery = q
	if f.threadErr != nil {
		return worklet.ResolvedThread{}, f.threadErr
	}
	out := f.thread
	if out.Asked == "" {
		out.Asked = q.Ask
	}
	if out.Answers == "" {
		out.Answers = q.Answers
	}
	return out, nil
}

// The PROJECT seam, which [builtin.ProjectReader] asserts for: a reader that
// answers the task questions and not these is a build with no Worklet,
// and the registration turns on exactly that.
func (f *fakeTracker) Projects(_ context.Context, q worklet.ProjectQuery) (
	worklet.ProjectListing, error) {

	f.projectQuery = q
	return f.projects, f.readErr
}

func (f *fakeTracker) CarriedProject(_ context.Context, q worklet.ProjectDetailQuery) (
	worklet.ProjectDetail, error) {

	f.detailQuery = q
	return f.project, f.readErr
}

// The FEED seam — what happened, and what is waiting on somebody. A reader
// that answers the task questions and not these is a build with no
// Worklet, and the registration turns on exactly that.
func (f *fakeTracker) Activity(_ context.Context, q worklet.ActivityQuery,
	_ time.Time) (worklet.ActivityAnswer, error) {

	f.activityQuery = q
	return f.activity, f.readErr
}

func (f *fakeTracker) MyWork(_ context.Context, q worklet.MyWorkQuery,
	_ time.Time, loc *time.Location) (worklet.MyWork, error) {

	f.myWorkQuery = q
	f.myWorkZone = loc
	return f.myWork, f.readErr
}

// as records the actor and hands back a writer bound to it, which is the
// tracker's own rule: a writer acts as exactly one party.
func (f *fakeTracker) as(actor builtin.Actor) builtin.WorkWriter {
	f.actors = append(f.actors, actor)
	return f
}

// depends is the same fake in its third shape, for the one gesture that is a
// SEQUENCE rather than a patch.
func (f *fakeTracker) depends(actor builtin.Actor) builtin.WorkDepender {
	f.actors = append(f.actors, actor)
	return f
}

// Search implements [builtin.WorkSearcher]: the fake in its fifth shape, for
// the one read here that is a RANKING rather than a filter.
func (f *fakeTracker) Search(_ context.Context,
	q worklet.SearchQuery) (worklet.SearchAnswer, error) {

	f.searched = append(f.searched, q.Text)
	f.searchModes = append(f.searchModes, q.Mode)
	if f.searchErr != nil {
		return worklet.SearchAnswer{}, f.searchErr
	}
	answer := worklet.SearchAnswer{}
	answer.ServedMode = q.Mode.Resolved()
	answer.Coverage.Complete = true
	if f.partialSearch {
		answer.Coverage = knowledge.Coverage{BucketsMissing: 21}
	}
	if f.searchDegraded != knowledge.NotDegraded {
		answer.Degraded = f.searchDegraded
		answer.ServedMode = knowledge.ModeKeyword
		if q.Mode.Resolved() == knowledge.ModeSemantic {
			answer.ServedMode = ""
			return answer, nil
		}
	}
	if q.Limit <= 0 || q.Limit > len(f.ranked) {
		answer.Hits = f.ranked
		return answer, nil
	}
	answer.Hits = f.ranked[:q.Limit]
	return answer, nil
}

// merges is its fourth, for the second sequence.
func (f *fakeTracker) merges(actor builtin.Actor) builtin.WorkMerger {
	f.actors = append(f.actors, actor)
	return f
}

// mergeCall is one fold as the tool composed it.
type mergeCall struct {
	duplicate, into string
	reparent        bool
	notify          *worklet.Notify
}

// MergeDuplicates records the fold, for the same reason Depend does: what the
// TOOL resolves and decides is the half that lives here, and the sequence
// itself is certified against a real store in the tracker's own suite.
func (f *fakeTracker) MergeDuplicates(_ context.Context, _ string,
	duplicate, into string, reparent bool,
	notify *worklet.Notify) (worklet.WriteResult, error) {

	f.merged = append(f.merged, mergeCall{
		duplicate: duplicate, into: into, reparent: reparent, notify: notify,
	})
	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	return worklet.WriteResult{Result: statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 51},
		Version:  51,
	}}, nil
}

// moves is its sixth, for the cross-project move.
func (f *fakeTracker) moves(actor builtin.Actor) builtin.WorkMover {
	f.actors = append(f.actors, actor)
	return f
}

// moveCall is one move as the tool composed it.
type moveCall struct {
	task, target string
	notify       *worklet.Notify
}

// MoveTaskToProject records the move — the sequence itself is certified
// against a real store in the tracker's own suite.
func (f *fakeTracker) MoveTaskToProject(_ context.Context, opID, taskID, target string,
	notify *worklet.Notify) (worklet.WriteResult, error) {

	f.moved = append(f.moved, moveCall{task: taskID, target: target, notify: notify})
	f.opIDs = append(f.opIDs, opID)
	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	return worklet.WriteResult{Key: target + "-3", Result: statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 71},
		Version:  71,
	}}, nil
}

func (f *fakeTracker) CreateTask(_ context.Context, opID string, task worklet.Task,
	notify *worklet.Notify) (worklet.WriteResult, error) {

	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	f.created = append(f.created, task)
	f.notified = append(f.notified, notify)
	f.opIDs = append(f.opIDs, opID)
	if f.createAnswer != nil {
		return *f.createAnswer, nil
	}
	// THE ASSIGNEE THE TASK WAS FILED TO, as the tracker reports it: the
	// named one, or this fake's stand-in for the project's default.
	assignee := task.Assignee
	if assignee == "" {
		assignee = f.defaultAssignee
	}
	return worklet.WriteResult{
		Key: "ENG-9", Assignee: assignee, Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 11},
		Version:  11,
	}, nil
}

// CreateTaskAsking records the ask beside the task, and is otherwise the
// create.
func (f *fakeTracker) CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
	ask worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error) {

	if f.writeErr == nil {
		f.asks = append(f.asks, ask)
	}
	return f.CreateTask(ctx, opID, task, notify)
}

func (f *fakeTracker) UpdateTask(_ context.Context, opID, _, _ string, ifMatch uint64,
	patch worklet.TaskPatch, kind worklet.ChangeKind,
	notify *worklet.Notify) (worklet.WriteResult, error) {

	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	f.patched = append(f.patched, patch)
	f.ifMatch = append(f.ifMatch, ifMatch)
	f.notified = append(f.notified, notify)
	f.kinds = append(f.kinds, kind)
	f.opIDs = append(f.opIDs, opID)
	return worklet.WriteResult{
		Key: "ENG-1", Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 12},
		Version:  12, Posted: f.posted,
	}, nil
}

// The PROJECT write side, which every surface has: declaring a tag is open to
// every seat, so a seat without it could never use the `labels` argument on
// the create and update tools it already holds.
func (f *fakeTracker) WriteProject(_ context.Context, opID, key string,
	edit worklet.ProjectEdit, authority worklet.ProjectAuthority) (
	worklet.WriteResult, error) {

	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	f.projectEdits = append(f.projectEdits, edit)
	f.projectAuthority = append(f.projectAuthority, authority)
	f.opIDs = append(f.opIDs, opID)
	return worklet.WriteResult{
		Result: statelog.Result{
			Outcome: statelog.OutcomeApplied, Version: 3,
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: 13},
		},
		Caveats: f.projectWarnings,
	}, nil
}

func (f *fakeTracker) WriteTags(_ context.Context, opID, project string,
	edit worklet.TagEdit, authority worklet.TagAuthority) (
	worklet.WriteResult, error) {

	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	f.tagEdits = append(f.tagEdits, edit)
	f.tagAuthority = append(f.tagAuthority, authority)
	f.opIDs = append(f.opIDs, opID)
	return worklet.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 4,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 14},
		Caveats:  f.tagWarnings,
	}, nil
}

// The CATALOGUE write side, which is the OPERATOR's alone. The declaration a
// tool composed is what a case here asserts; whether the tracker accepts it is
// the tracker's own suite (internal/worklet/config_test.go), and a second copy
// of that gate in this fake would certify nothing but itself.
func (f *fakeTracker) WriteTypes(_ context.Context, opID string,
	types []worklet.TaskType) (worklet.WriteResult, error) {

	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	f.declaredTypes = append(f.declaredTypes, types)
	f.opIDs = append(f.opIDs, opID)
	return worklet.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 5,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 15},
	}, nil
}

func (f *fakeTracker) WriteFields(_ context.Context, opID string,
	fields []worklet.FieldDef) (worklet.WriteResult, error) {

	if f.writeErr != nil {
		return worklet.WriteResult{}, f.writeErr
	}
	f.declaredFields = append(f.declaredFields, fields)
	f.opIDs = append(f.opIDs, opID)
	return worklet.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 6,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 16},
	}, nil
}

func (f *fakeTracker) EnsureTags(_ context.Context, opID, project string,
	tags []string) ([]string, []caveat.Caveat, error) {

	if f.writeErr != nil {
		return nil, nil, f.writeErr
	}
	if f.ensureErr != nil {
		f.opIDs = append(f.opIDs, opID)
		return nil, nil, f.ensureErr
	}
	f.ensured = append(f.ensured, tags)
	f.ensuredIn = append(f.ensuredIn, project)
	f.opIDs = append(f.opIDs, opID)
	return tags, f.tagWarnings, nil
}

type fakeMentions []string

func (f fakeMentions) Mentions(string) []string { return f }

// workRegistry registers a seat's tracker tools over a fake tracker.
func workRegistry(t *testing.T, deps builtin.WorkDeps) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Work: deps})); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// workTurn is a turn context bound to a seat, as the tool surface binds it.
func workTurn(t *testing.T) *turnctx.Turn {
	t.Helper()
	o := &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}},
	}
	o.Normalize()
	return &turnctx.Turn{
		RunID: "run-1", WorkKey: "turn-1",
		Seat: o.Roles[0], Org: o, Chain: []string{"pm"},
	}
}

func callWork(t *testing.T, reg *tools.Registry, name string, args map[string]any) tools.Result {
	t.Helper()
	return callWorkAs(everyGrant(), t, reg, name, args)
}

// callWorkAs is [callWork] for a case that is about WHO is calling.
//
// A CASE ABOUT AUTHORITY MUST NOT USE [everyGrant], which carries
// admin — the permission every relation class checks BEFORE the chart, so
// a fixture holding it is allowed by a chart that refuses everything and the
// case asserts nothing.
func callWorkAs(ctx context.Context, t *testing.T, reg *tools.Registry,
	name string, args map[string]any) tools.Result {

	t.Helper()
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	seatCallable, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s is not seat-callable", name)
	}
	got, err := seatCallable.CallForTurn(ctx, workTurn(t), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// ---- the cases -------------------------------------------------------- //

// THE FIVE WRITES COUNT AS A DELIVERY. A turn woken by an assignment answers by
// moving the item, commenting on it, filing the follow-up, or merging it into
// the item it duplicates — and without this the gate sees only builtins,
// concludes the turn reached nobody, and corrects it into another round.
func TestTheTrackerWritesCountAsDeliveries(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves})

	deliveries := reg.Deliveries()
	for _, name := range builtin.WorkWrites() {
		// AND ON THE TRACKER. The name alone says the turn reached
		// somebody; the surface says whom, and without it a tracker write
		// discharged an obligation owed to a founder waiting in chat.
		if deliveries[name] != worklet.Source {
			t.Errorf("%s delivers to %q, want %q — a turn that answered with it "+
				"would otherwise be corrected for having done nothing, or credited "+
				"for answering somebody it never reached",
				name, deliveries[name], worklet.Source)
		}
	}
	// READING IS NOT DELIVERING. A turn that only read is exactly the turn
	// the gate exists to catch.
	for _, name := range []string{builtin.ListWorkItemsTool, builtin.GetWorkItemTool} {
		if _, ok := deliveries[name]; ok {
			t.Errorf("%s counts as a delivery, so a turn that only read would "+
				"pass the gate", name)
		}
	}
}

// IDENTITY COMES FROM THE TURN, NEVER FROM ARGUMENTS: a model that could name
// its own actor could file work as anybody. An argument the tool does not read
// is refused by name, so the forged call writes nothing at all.
func TestAWriteIsAttributedToTheTurnsSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	forged := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "new work", "project": "ENG",
		// A model trying to act as somebody else.
		"actor": "ceo", "reporter": "ceo", "handle": "ceo",
	})
	if !forged.Failed || !errors.Is(forged.Cause, builtin.ErrUndeclaredArgument) {
		t.Fatalf("a create naming its own actor answered %q (cause %v), want "+
			"the arguments refused by name", forged.Output, forged.Cause)
	}
	for _, named := range []string{`"actor"`, `"reporter"`, `"handle"`} {
		if !strings.Contains(forged.Output, named) {
			t.Errorf("the refusal does not name %s: %s", named, forged.Output)
		}
	}
	if len(trk.actors) != 0 {
		t.Fatalf("the forged create wrote as %v", trk.actors)
	}

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "new work", "project": "ENG",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	if len(trk.actors) != 1 {
		t.Fatalf("actors = %v", trk.actors)
	}
	actor := trk.actors[0]
	if actor.Handle != "eng" || actor.Kind != worklet.AuthorAgent {
		t.Errorf("attributed to %+v, want the turn's own seat", actor)
	}
	// THE RUN is the provenance an audit walks back to, and the WORK KEY is
	// what the derived operation and comment ids are seeded from — a
	// redelivered trigger runs again under a new run, and an id seeded from
	// that would post the same comment twice. See ADR-0017.
	if actor.TurnID != "run-1" || !slices.Equal(actor.Chain, []string{"pm"}) {
		t.Errorf("provenance = %+v, want the run and the chain", actor)
	}
	if actor.WorkKey != "turn-1" || actor.OperationSeed() != "turn-1" {
		t.Errorf("provenance = %+v, want the unit of work as the id seed", actor)
	}
}

// A TASK FILED BY A TURN A CHAT MESSAGE WOKE SAYS WHERE IT CAME FROM — the
// surface and the conversation, off the turn and never off an argument (the
// tool declares none, and an undeclared one is refused by name before the tool
// runs) — and one filed by a turn nothing on a chat surface woke says nothing.
func TestAWriteCarriesTheChatSurfaceTheTurnWasWokenOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		transport string
		want      *worklet.Origin
	}{
		{"slack", &worklet.Origin{Surface: "slack", Conversation: "slack:C1:171.2"}},
		{"", nil},
	} {
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		turn := workTurn(t)
		turn.Transport, turn.ConversationKey = tc.transport, "slack:C1:171.2"
		entry, _ := reg.Lookup(builtin.CreateWorkItemTool)
		got, err := entry.Tool.(tools.SeatCallable).CallForTurn(t.Context(), turn,
			map[string]any{"title": "from the thread", "project": "ENG"})
		if err != nil || got.Failed {
			t.Fatalf("create: %v %s", err, got.Output)
		}
		origin := trk.actors[0].Thread
		if (origin == nil) != (tc.want == nil) ||
			(origin != nil && *origin != *tc.want) {
			t.Errorf("a turn woken on %q filed with origin %+v, want %+v",
				tc.transport, origin, tc.want)
		}
		if provenance := trk.actors[0].Provenance(); provenance.Origin != origin {
			t.Error("the writer's provenance does not carry the actor's origin")
		}
	}
}

// A WRITE'S ANSWER NAMES WHERE IT LANDED, in the form every read grammar
// takes back as `min_position` — which is the whole of read-your-writes for a
// caller outside the engine: an operator's assistant that created a task
// through these same tools holds nothing else it could ask a board to include.
func TestAWriteAnswersWithThePositionItLandedAt(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "new work", "project": "ENG",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer is not json: %v", err)
	}
	// THE FAKE LANDS EVERY CREATE AT S@1:11, and the answer says so in the
	// parseable form rather than as three numbers to reassemble.
	if answer["position"] != "S@1:11" {
		t.Errorf("the answer carries position %v, want %q — a write that "+
			"does not say where it landed leaves its caller nothing to "+
			"read back at", answer["position"], "S@1:11")
	}
	if answer["outcome"] != string(statelog.OutcomeApplied) {
		t.Errorf("outcome = %v, want applied beside the position", answer["outcome"])
	}
}

// OUTSIDE A TURN THERE IS NO SEAT, so every one of these refuses rather than
// writing as nobody.
func TestTheTrackerToolsRefuseOutsideATurn(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	files := newFakeFiles()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves, Search: trk,
		ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
		Files:         files, FileWriter: files.as, Objects: newFakeObjects(),
	})
	for _, name := range builtin.WorkTools() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		got, err := entry.Tool.Call(everyGrant(), declaredOf(entry.Tool,
			map[string]any{"item": "ENG-1", "title": "x", "body": "y"}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Failed || !strings.Contains(got.Output, "during a turn") {
			t.Errorf("%s outside a turn gave %q", name, got.Output)
		}
	}
}

// A COMPANY RUNNING JIRA GETS NO WORKLET TOOLS AT ALL. A seat offered a tool
// against a tracker its company does not run would reach for it and fail at
// the call, and learn to distrust the whole catalogue.
func TestNoTrackerMeansNoTools(t *testing.T) {
	t.Parallel()
	reg := workRegistry(t, builtin.WorkDeps{})
	for _, name := range builtin.WorkTools() {
		if _, ok := reg.Lookup(name); ok {
			t.Errorf("%s was registered with no tracker configured", name)
		}
	}
}

// A COMMENT FROM A TURN CARRIES THE TURN'S KEY, which is what makes a re-run
// turn post once rather than saying the same thing twice.
func TestACommentCarriesTheTurnsKeyAndItsMentions(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Mentions: fakeMentions{"pm"},
	})
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "body": "@pm this needs a decision",
	})
	if got.Failed {
		t.Fatalf("comment failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil {
		t.Fatalf("the comment did not ride the task's own write: %+v", trk.patched)
	}
	comment := trk.patched[0].Comment
	if !slices.Equal(comment.Mentions, []string{"pm"}) {
		t.Errorf("mentions = %v, want the resolved handles", comment.Mentions)
	}

	// THE ID IS DERIVED FROM THE TURN, which is what makes a re-run turn
	// post once: the second call carries the same primary key, so the
	// applier's upsert overwrites rather than appending a duplicate.
	again := newFakeTracker()
	reg2 := workRegistry(t, builtin.WorkDeps{
		Reader: again, Writer: again.as, Mentions: fakeMentions{"pm"},
	})
	if got := callWork(t, reg2, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "body": "@pm this needs a decision",
	}); got.Failed {
		t.Fatalf("the re-run comment failed: %s", got.Output)
	}
	if again.patched[0].Comment.ID != comment.ID {
		t.Errorf("a re-run turn minted a second comment id (%s then %s), so it "+
			"would say the same thing twice", comment.ID, again.patched[0].Comment.ID)
	}
	// AND THE OPERATION ID WITH IT, so the ledger collapses the retry
	// even before the applier sees the duplicate key.
	if again.opIDs[0] != trk.opIDs[0] {
		t.Errorf("a re-run turn minted a second operation id (%s then %s)",
			trk.opIDs[0], again.opIDs[0])
	}
}

// A BAD ENUM IS REFUSED WITH THE LIST, so the model's next call is right
// rather than being another guess: every value, each quoted as the refused one
// beside it is, read whole.
func TestABadEnumIsRefusedWithTheValidValues(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	// THE CLOSE REASON IS THE STATUS. The tracker has no separate field:
	// `cancelled` IS "finished without being delivered", which is what
	// makes it invisible to a delivery count with no second value to keep in step.
	statuses := "The statuses are: " + words.And(worklet.Statuses) + "."
	priorities := "The priorities are: " + words.And(worklet.Priorities) + "."
	for _, tc := range []struct {
		tool       string
		args       map[string]any
		want, list string
	}{
		{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "status": "wip"}, "in_progress", statuses},
		{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "status": "wontfix"}, "cancelled", statuses},
		{builtin.UpdateWorkItemTool, map[string]any{"item": "ENG-1", "priority": "P0"}, "urgent", priorities},
		{builtin.CreateWorkItemTool, map[string]any{"title": "somewhere", "project": "ENG", "priority": "P0"},
			"urgent", priorities},
	} {
		got := callWork(t, reg, tc.tool, tc.args)
		if !got.Failed {
			t.Errorf("%s %v was accepted", tc.tool, tc.args)
			continue
		}
		if !strings.Contains(got.Output, tc.want) || !strings.Contains(got.Output, tc.list) {
			t.Errorf("the refusal of %s %v does not list the valid values as %q: %s",
				tc.tool, tc.args, tc.list, got.Output)
		}
	}
}

// A FAILED READ MUST NEVER READ AS "NOTHING FOUND". A projection that has not
// caught up telling a seat the company has no work makes it file a duplicate
// or abandon work it was told to do.
func TestAFailedReadIsNotAnEmptyResult(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.readErr = errors.New("the projection is not hydrated yet")
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	for _, name := range []string{builtin.ListWorkItemsTool, builtin.GetWorkItemTool} {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		got := callWork(t, reg, name, declaredOf(entry.Tool,
			map[string]any{"item": "ENG-1"}))
		if !got.Failed {
			t.Errorf("%s reported success on a failed read", name)
		}
		if !strings.Contains(got.Output, "NOT an empty result") {
			t.Errorf("%s does not say the difference: %s", name, got.Output)
		}
	}
}

// A READ REFUSED FOR WHAT IT ASKED IS NOT "TRY AGAIN". A feed's cursor that
// is not a position, a cursor or `since` from another log, a text search wider
// than the feed scans, a floor on another log: each is refused the same on
// every attempt, so the refusal says the arguments are what has to change.
// The control is the case above, where the store failed and trying again is
// right.
func TestARequestTheTrackerRefusedIsNotToldToTryAgain(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{
		fmt.Errorf("%w: cursor: \"42\" is not a log position", worklet.ErrBadQuery),
		fmt.Errorf("this read's floor: %w", statelog.ErrForeignPosition),
	} {
		trk := newFakeTracker()
		trk.readErr = refusal
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		for _, name := range []string{builtin.ListWorkItemsTool, builtin.GetWorkItemTool} {
			entry, ok := reg.Lookup(name)
			if !ok {
				t.Fatalf("%s is not registered", name)
			}
			got := callWork(t, reg, name, declaredOf(entry.Tool,
				map[string]any{"item": "ENG-1"}))
			if !got.Failed {
				t.Errorf("%s reported success on a refused read", name)
			}
			if strings.Contains(got.Output, "Try again") ||
				!strings.Contains(got.Output, "change the arguments") {
				t.Errorf("%s on %v says: %s — want it to name the arguments as "+
					"what changes, never to try again", name, refusal, got.Output)
			}
		}
	}
}

// A CURSOR THE TRACKER REFUSES IS DROPPED, NEVER RETRIED. A listing sorted by
// the org chart or the type catalogue refuses a cursor minted before either
// was edited, on every attempt, so the tool names the argument to drop:
// `cursor` on a listing, `comments_cursor` on an item.
//
// Mutation: drop either tool's ErrBadCursor arm and its case goes red.
func TestARefusedCursorIsToldToBeDropped(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.readErr = fmt.Errorf("%w: it was minted for another order", worklet.ErrBadCursor)
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	for name, drop := range map[string]string{
		builtin.ListWorkItemsTool: "without `cursor`",
		builtin.GetWorkItemTool:   "without `comments_cursor`",
	} {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		got := callWork(t, reg, name, declaredOf(entry.Tool,
			map[string]any{"item": "ENG-1"}))
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			!strings.Contains(got.Output, drop) || strings.Contains(got.Output, "worklet:") {
			t.Errorf("%s on a refused cursor says %q (%s), want it to ask again %s",
				name, got.Output, tools.RefusalOf(got), drop)
		}
	}
}

// A FAILED WRITE SAYS THE CHANGE WAS NOT MADE, or the model reports work it
// did not do — and one that met a FAULT of the node says it was not done
// without claiming either way, and keeps the fault's own words out of the
// sentence: those are a store's, and go to the log.
func TestAFailedWriteSaysSo(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = &statelog.Unavailable{Reason: statelog.ReasonLogFull,
		Detail: "the broker refused to store the record"}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || !strings.Contains(got.Output, "NOT made") ||
		tools.RefusalOf(got) != tools.RefusalUnavailable {
		t.Errorf("a refused write gave %q (%s)", got.Output, tools.RefusalOf(got))
	}

	trk.writeErr = errors.New("worklet: read the re-spread window: open " +
		"/var/lib/crewlet/replicated.db: disk I/O error")
	got = callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalInternalError ||
		!strings.Contains(got.Output, "Do not report it as done") ||
		strings.Contains(got.Output, "/var/lib") || strings.Contains(got.Output, "Try again") {
		t.Errorf("a write that met a fault gave %q (%s)", got.Output, tools.RefusalOf(got))
	}

	// AND THE HAND-OFF BUDGET TELLS IT WHAT TO DO INSTEAD, rather than
	// inviting another attempt.
	trk.writeErr = worklet.ErrReassignmentBudget
	got = callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "assignee": "ops",
	})
	if !strings.Contains(got.Output, "Do not reassign it again") {
		t.Errorf("the budget refusal invites another attempt: %q", got.Output)
	}

	// AND A WALK THAT STOPPED AT AN UNKNOWN STEP IS NEITHER: some of it may
	// have landed, so "NOT made" would be false, and the same call again is
	// what finishes it.
	trk.writeErr = fmt.Errorf("stopped: %w", worklet.ErrStepUnresolved)
	got = callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || strings.Contains(got.Output, "NOT made") ||
		!strings.Contains(got.Output, "exactly the same arguments") {
		t.Errorf("a walk that stopped at an unresolved step gave %q", got.Output)
	}
}

// A CREATE WHOSE OUTCOME IS UNKNOWN IS NEVER REPORTED AS FILED, NOR AS NOT
// MADE, and what it tells the caller to do is what a repeat would actually be:
// a receipt's key reads as the item's even if the task step never landed, and
// "NOT made" about an item a first run filed makes the seat reword the call
// and file a duplicate.
func TestACreateWhoseOutcomeIsUnknownIsNeverReportedAsFiled(t *testing.T) {
	t.Parallel()
	lostAck := worklet.WriteResult{
		Key:    "ENG-9",
		Result: statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op.task"},
	}
	unvouched := worklet.WriteResult{Result: statelog.Result{
		Outcome: statelog.OutcomeUnknown, OpID: "op.counter", Unvouched: true,
	}}
	cases := []struct {
		name     string
		operator bool
		answer   worklet.WriteResult
		want     []string
		refuse   []string
	}{
		{
			name: "a seat under a lost acknowledgement repeats the same call", answer: lostAck,
			want: []string{"exactly the same arguments", "Do not reword it",
				"If this attempt filed it, it is ENG-9"},
			refuse: []string{"cannot tell", "list_work_items"},
		},
		{
			name: "a seat the ledger cannot vouch for looks before it refiles", answer: unvouched,
			want: []string{"this node cannot tell", "list_work_items",
				"Never make it again under different arguments"},
			refuse: []string{"If this attempt filed it", "acknowledgement was lost"},
		},
		{
			name: "an operator's repeat is a new operation", operator: true, answer: lostAck,
			want:   []string{"list_work_items", "files a second item"},
			refuse: []string{"exactly the same arguments", "cannot tell"},
		},
		{
			name: "an operator the ledger cannot vouch for looks first", operator: true,
			answer: unvouched,
			want:   []string{"this node cannot tell", "list_work_items"},
			refuse: []string{"exactly the same arguments"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.createAnswer = &tc.answer
			deps := builtin.WorkDeps{Reader: trk, Writer: trk.as}
			call := callWork
			if tc.operator {
				deps.Actor, call = operatorActor, callNoTurn
			}
			got := call(t, workRegistry(t, deps), builtin.CreateWorkItemTool,
				map[string]any{"title": "the follow-up", "project": "ENG"})
			if !got.Failed {
				t.Fatalf("an unknown create answered as a receipt: %s", got.Output)
			}
			for _, bad := range []string{"NOT made", `"key"`} {
				if strings.Contains(got.Output, bad) {
					t.Errorf("the answer carries %q, which is a claim about an "+
						"item nobody can say was filed: %s", bad, got.Output)
				}
			}
			if !strings.Contains(got.Output, trk.opIDs[0]) {
				t.Errorf("the answer does not name operation %s: %s",
					trk.opIDs[0], got.Output)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the answer lacks %q: %s", want, got.Output)
				}
			}
			for _, bad := range tc.refuse {
				if strings.Contains(got.Output, bad) {
					t.Errorf("the answer says %q, which is not what a repeat "+
						"here would be: %s", bad, got.Output)
				}
			}
		})
	}
}

// A WALK THIS NODE CANNOT VOUCH FOR IS NOT ANSWERED "CALL AGAIN". The same
// call re-runs under the same operation, and the step it stopped at needs the
// ledger row the loss took — so on this node it stops there every time, and a
// model told to repeat it loops until its rounds run out.
func TestAWalkThisNodeCannotVouchForIsNotAnsweredWithARetry(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	trk.writeErr = fmt.Errorf("stopped: %w", worklet.ErrStepUnvouched)
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || strings.Contains(got.Output, "NOT made") ||
		strings.Contains(got.Output, "exactly the same arguments") ||
		!strings.Contains(got.Output, "stops at the same step") {
		t.Errorf("a walk this node cannot vouch for gave %q", got.Output)
	}
}

// A REFERENCE IS RESOLVED TO AN ID BEFORE IT IS STORED. A model types the key
// it read; a relation and a parent pointer are ids, and a key stored in either
// is an edge that resolves to nothing on every node forever.
func TestAReferenceIsResolvedRatherThanStoredAsTyped(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "duplicate_of": "eng-1", "status": "cancelled",
	}); got.Failed {
		t.Fatalf("the duplicate link failed: %s", got.Output)
	}
	// A GESTURE, AND NEVER THE WHOLE SET: [worklet.TaskPatch]'s collections
	// are carried whole, so this one edge as the collection would delete every
	// other relation the item has — and a deleted `waiting_on` edge leaves its
	// mirror on the blocker, which the repair cannot find from the AUTHORED end.
	gesture := trk.patched[0].Relate
	if trk.patched[0].Relations != nil {
		t.Fatalf("the tool stated the whole relation set %v, which is every "+
			"other edge this item had being deleted",
			*trk.patched[0].Relations)
	}
	if gesture == nil || len(gesture.Add) != 1 {
		t.Fatalf("the relation gesture is %v", gesture)
	}
	if got := gesture.Add[0].Other; got != "i1" {
		t.Errorf("the link stored %q — the id is what the mirror edge and "+
			"every board render from, so a key here is a dead reference", got)
	}

	if got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "child", "project": "ENG", "parent": "ENG-1",
	}); got.Failed {
		t.Fatalf("the child failed: %s", got.Output)
	}
	if trk.created[0].Parent == nil || *trk.created[0].Parent != "i1" {
		t.Errorf("parent = %v, want the resolved id — the applier derives the "+
			"subtree's root and depth from it", trk.created[0].Parent)
	}

	// AND A REFERENCE THAT RESOLVES TO NOTHING IS REFUSED, rather than
	// stored for a duty to retry forever.
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "duplicate_of": "ENG-404",
	})
	if !got.Failed || !strings.Contains(got.Output, "no such work item") {
		t.Errorf("a dangling duplicate_of gave %q", got.Output)
	}
}

// A SUBTASK FILES INTO ITS PARENT'S PROJECT when the call names none: filed
// in the caller's team it would be on neither project's board, and the
// tracker refuses a subtask outside its parent's project. A plain item still
// takes the caller's team.
func TestASubtaskFilesIntoItsParentsProjectByDefault(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		DefaultProject: func(string) string { return "OPS" },
	})
	if got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "one piece of it", "parent": "ENG-1",
	}); got.Failed {
		t.Fatalf("the subtask failed: %s", got.Output)
	}
	if got := trk.created[0].Project; got != "ENG" {
		t.Errorf("a subtask of ENG-1 was filed under %q, want ENG — its "+
			"parent's project, not the caller's team", got)
	}
	if got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "something else",
	}); got.Failed {
		t.Fatalf("the plain item failed: %s", got.Output)
	}
	if got := trk.created[1].Project; got != "OPS" {
		t.Errorf("a plain item was filed under %q, want the caller's team OPS", got)
	}
}

// EVERY INERT EDGE ONE CALL STATES TRAVELS IN ONE GESTURE. A patch carries
// exactly one relation gesture: two arms composing two gestures means the
// last wins and the other's edges are silently gone, and one gesture plus one
// whole SET is refused by the writer outright, as a programming error the
// model cannot fix.
func TestOneCallsLinkAndDuplicateBothSurvive(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "duplicate_of": "ENG-1",
		"linked": map[string]any{"add": []any{"ENG-1"}},
		"status": "cancelled",
	}); got.Failed {
		t.Fatalf("a link and a duplicate in one call failed: %s", got.Output)
	}
	gesture := trk.patched[0].Relate
	if gesture == nil {
		t.Fatal("the call carried no relation gesture at all")
	}
	var kinds []worklet.RelationKind
	for _, add := range gesture.Add {
		kinds = append(kinds, add.Kind)
	}
	for _, want := range []worklet.RelationKind{
		worklet.RelationLinked, worklet.RelationDuplicates,
	} {
		if !slices.Contains(kinds, want) {
			t.Errorf("the gesture adds %v and not %q — the arm that ran "+
				"second replaced the first one's edges rather than joining "+
				"them", kinds, want)
		}
	}
}

// A DUPLICATE LINK COMES OFF THE WAY AN ASSIGNEE DOES: null or "" clears the
// item's own duplicate edges, or a link drawn by mistake stays on both tasks
// for good.
func TestADuplicateLinkIsTakenOffByAnEmptyDuplicateOf(t *testing.T) {
	t.Parallel()
	for _, cleared := range []any{nil, "", "  "} {
		trk := newFakeTracker()
		one := trk.tasks["ENG-1"]
		one.Task.Relations = []worklet.Relation{
			{Kind: worklet.RelationDuplicates, Other: "id-2"},
			{Kind: worklet.RelationLinked, Other: "id-3"},
		}
		trk.tasks["ENG-1"] = one
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

		if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "duplicate_of": cleared,
		}); got.Failed {
			t.Fatalf("duplicate_of %#v failed: %s", cleared, got.Output)
		}
		if len(trk.patched) != 1 || trk.patched[0].Relate == nil {
			t.Fatalf("duplicate_of %#v wrote no relation gesture: %+v", cleared, trk.patched)
		}
		gesture := trk.patched[0].Relate
		want := []worklet.Relation{{Kind: worklet.RelationDuplicates, Other: "id-2"}}
		if len(gesture.Add) != 0 || !slices.Equal(gesture.Remove, want) {
			t.Errorf("duplicate_of %#v: the gesture adds %v and removes %v, want "+
				"the duplicate edge removed and the link left alone",
				cleared, gesture.Add, gesture.Remove)
		}
	}

	// AN ITEM THAT MARKS NOTHING A DUPLICATE HAS NOTHING TO CLEAR, and the
	// call changes nothing rather than writing an empty gesture.
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "duplicate_of": nil, "status": "in_progress",
	})
	for _, patch := range trk.patched {
		if patch.Relate != nil {
			t.Errorf("clearing a duplicate link the item does not have wrote %+v", patch.Relate)
		}
	}
}

// IF-MATCH IS PASSED THROUGH AS THE MODEL'S OWN PRECONDITION, and omitting it
// merges — which is what a model naming two fields needs.
func TestIfMatchReachesTheWriter(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "in_progress", "if_match": 7,
	})
	callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "in_progress",
	})
	if !slices.Equal(trk.ifMatch, []uint64{7, worklet.NoIfMatch}) {
		t.Errorf("if_match reached the writer as %v, want [7 0]", trk.ifMatch)
	}
}

// A STALE VERSION TELLS THE MODEL TO RE-READ. Told only "it failed", a model
// re-sends the same patch against the same moved item forever.
func TestAStaleVersionSaysToReadItAgain(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = fmt.Errorf("%w: task i1 is at version 9", worklet.ErrStaleVersion)
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done", "if_match": 7,
	})
	if !got.Failed {
		t.Fatal("a stale edit reported success")
	}
	if !strings.Contains(got.Output, "get_work_item") {
		t.Errorf("the refusal does not say to re-read: %q", got.Output)
	}
	// AND IT SAYS SO TO A READER THAT IS NOT A MODEL: the class is what a
	// person's surface re-reads and re-offers on, where a lost race is
	// retried as it stands.
	if class := tools.RefusalOf(got); class != tools.RefusalStaleVersion {
		t.Errorf("a stale edit is classed %q, want %q", class,
			tools.RefusalStaleVersion)
	}
}

// THE ANNOTATIONS ARE WHAT THE WORKER GUARD READS. A write to a surface the
// whole company sees must not be reachable by a sub-agent acting under its
// parent's name.
func TestTheTrackerWritesAreClassifiedAsSharedWrites(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves})

	for _, name := range builtin.WorkWrites() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		if !mcp.WritesToSharedSurface(entry.Annotations) {
			t.Errorf("%s is not classified as a shared write, so a sub-agent "+
				"could file work under its parent's name", name)
		}
	}
	for _, name := range []string{builtin.ListWorkItemsTool, builtin.GetWorkItemTool} {
		entry, _ := reg.Lookup(name)
		if !mcp.ReadOnlyProven(entry.Annotations) {
			t.Errorf("%s is not proven read-only, so the delivery gate would "+
				"count reading as delivering", name)
		}
	}
	// UPDATE IS DESTRUCTIVE: it replaces a title, a description or an
	// assignee, and the previous value survives only in the change record.
	entry, _ := reg.Lookup(builtin.UpdateWorkItemTool)
	if entry.Annotations.Destructive != mcp.Yes {
		t.Error("update_work_item is not marked destructive, though it replaces " +
			"somebody's description with no undo outside the change record")
	}
}

// EVERY ARGUMENT A MODEL SENDS REACHES THE GRAMMAR, under the grammar's own
// name. The tool's arguments are the model's vocabulary and the query keys the
// transport's, and a translation nothing checks silently stops translating:
// `text` copied through under its own name, which the grammar does not read,
// returns an unfiltered list, and an `open_only` naming groups that do not
// exist makes the parser refuse the whole call.
func TestEveryListArgumentReachesTheGrammar(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{
		"project": "eng", "assignee": "swe", "label": "urgent",
		"status": []any{"todo"}, "text": "deploy", "limit": 5,
	})
	if got.Failed {
		t.Fatalf("the list refused a filter it offers: %s", got.Output)
	}
	q := trk.query
	switch {
	case q.Scope.Project != "ENG":
		t.Errorf("project reached the grammar as %q", q.Scope.Project)
	case len(q.Assignee) != 1 || q.Assignee[0] != "swe":
		t.Errorf("assignee reached the grammar as %v", q.Assignee)
	case len(q.Tags.Tags) != 1 || q.Tags.Tags[0] != "urgent":
		t.Errorf("label reached the grammar as %v", q.Tags.Tags)
	case len(q.Status) != 1 || q.Status[0] != worklet.StatusTodo:
		t.Errorf("status reached the grammar as %v", q.Status)
	case q.Text != "deploy":
		t.Errorf("text reached the grammar as %q — a model's search term was "+
			"dropped and the answer widened to everything", q.Text)
	case q.Limit != 5:
		t.Errorf("limit reached the grammar as %d", q.Limit)
	}

	// OPEN_ONLY IS THE TWO OPEN GROUPS, and there are exactly four groups.
	got = callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{
		"open_only": true,
	})
	if got.Failed {
		t.Fatalf("open_only failed the whole call: %s", got.Output)
	}
	want := []worklet.StatusGroup{worklet.GroupNotStarted, worklet.GroupActive}
	if len(trk.query.StatusGroups) != len(want) {
		t.Fatalf("open_only reached the grammar as %v, want %v",
			trk.query.StatusGroups, want)
	}
	for i, group := range want {
		if trk.query.StatusGroups[i] != group {
			t.Fatalf("open_only reached the grammar as %v, want %v",
				trk.query.StatusGroups, want)
		}
	}
}

// NO SEAT HOLDS AN OPERATOR-ONLY TOOL. Each is a decision a PERSON makes about
// how the company runs — its vocabulary, a tab strip, somebody's queue and
// inbox — so a seat holding one edits the rules it is judged by: a create
// refused for an undeclared type is a signal a person must see, and a seat
// has a mailbox, not an inbox.
//
// The guard is STRUCTURAL rather than a list kept here, because a list beside
// the registry is the thing that stops matching it.
func TestNoSeatHoldsAnOperatorOnlyTool(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	// EVERY WRITE SIDE WIRED, so a tool missing from the seat's registry
	// is missing because the registry does not offer it rather than
	// because its dependency was nil.
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves, Search: trk,
		ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
		CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
		PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
		TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
		Placer:          func(builtin.Actor) builtin.WorkPlacer { return nil },
		ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return trk },
	})
	for _, name := range worklet.OperatorOnlyTools() {
		if _, held := reg.Lookup(name); held {
			t.Errorf("a seat holds %s, which is a decision a person makes "+
				"about how the company runs", name)
		}
	}

	// AND THE OPERATOR SURFACE HOLDS EVERY ONE, or the split would be a
	// rule that removed a tool rather than one that placed it.
	operator := map[string]bool{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves, Search: trk,
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
			Placer:          func(builtin.Actor) builtin.WorkPlacer { return nil },
			ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return trk },
			Inbox:           trk,
			Files:           newFakeFiles(),
			FileWriter:      func(builtin.Actor) builtin.FileWriter { return newFakeFiles() },
			Objects:         newFakeObjects(),
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		operator[tool.Name()] = true
	}
	for _, name := range worklet.OperatorOnlyTools() {
		if !operator[name] {
			t.Errorf("%s is operator-only and the operator surface does not "+
				"serve it, so nothing serves it at all", name)
		}
	}
	// AND THE SEAT'S OWN ARE THERE TOO: the operator surface is the
	// same tools with one field different, not a second catalogue.
	for _, name := range worklet.Tools() {
		if !operator[name] {
			t.Errorf("the operator surface does not serve %s, which every "+
				"seat holds", name)
		}
	}
}

// A SEAT IS NOT TOLD A POPULATED BOARD IS EMPTY. A grouped answer has no
// flat rows by construction, so an empty message that asked only about those
// would call a board of five columns empty, and a seat believing it would
// file the duplicate.
func TestAGroupedAnswerIsNotReportedAsEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.answer = &worklet.Answer{
		Groups: []worklet.Group{{
			Key: "todo", Count: 12,
			Rows: []worklet.TaskRow{{ID: "t-1", Key: "ENG-1"}},
		}},
		Complete: true,
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{
		"project": "eng",
	})
	if strings.Contains(got.Output, "No work items match") {
		t.Fatalf("a board with a populated column was reported empty: %s",
			got.Output)
	}
	if !strings.Contains(got.Output, "groups") {
		t.Fatalf("the grouped half never reached the model: %s", got.Output)
	}
}

// AND A BOARD OF EMPTY COLUMNS IS EMPTY. A closed axis carries every column
// the query admits, full or not, so a seat handed three columns at zero and
// told nothing would read them as a board and go looking for the rows.
func TestABoardWhoseEveryColumnIsEmptyIsReportedEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.answer = &worklet.Answer{
		Groups: []worklet.Group{
			{Key: "todo", Rows: []worklet.TaskRow{}},
			{Key: "in_progress", Rows: []worklet.TaskRow{}},
			{Key: "in_review", Rows: []worklet.TaskRow{}},
		},
		Complete: true,
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{
		"project": "eng",
	})
	if !strings.Contains(got.Output, "No work items match") {
		t.Fatalf("three columns at zero were reported as a board: %s", got.Output)
	}
}

// EVERY OPERATOR TOOL ANSWERS WITHOUT A TURN. The operator surface calls
// through Callable.Call with a nil turn and supplies its own Actor, so a tool
// calling `turn.RequireSeat()` before consulting that Actor refuses every
// operator call — and a catalogue whose reads all fail is one an assistant
// stops trusting.
//
// The guard is structural: it calls EVERY tool the surface serves, so a tool
// added later with the same prelude fails here rather than in production.
func TestEveryOperatorToolAnswersOutsideATurn(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	work := builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
		CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
		PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
		TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
		Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
			return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator}, nil
		},
	}
	// AN AUTHORIZER THAT ADMITS, because a nil one refuses every call before
	// the tool runs — and a refusal is not the turn prelude this is about, so
	// without one the case passed having reached nothing.
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: work, Authorize: builtin.Decide(chartLeads),
	}) {
		// THE READS ONLY. A write called with no arguments refuses on
		// its own missing arguments, which is correct and says nothing
		// about the turn; what this case is about is the prelude that
		// refuses BEFORE any argument is read.
		if !strings.HasPrefix(tool.Name(), "list_") &&
			!strings.HasPrefix(tool.Name(), "get_") {
			continue
		}
		result, err := tool.Call(everyGrant(), map[string]any{})
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		if errors.Is(result.Cause, builtin.ErrNoAuthorizer) ||
			errors.Is(result.Cause, builtin.ErrRefused) {
			t.Fatalf("%s was refused before it ran (%s), so this case reached "+
				"nothing", tool.Name(), result.Output)
		}
		if strings.Contains(result.Output, "can only be called during a turn") {
			t.Errorf("%s refuses outside a turn even though the surface "+
				"supplies its own actor — the identity check must be "+
				"WorkDeps.actor, not turn.RequireSeat", tool.Name())
		}
	}
}

// Inbox answers one notice, which is all the placement test needs: what it is
// about is whether the surface SERVES the verb, never what the verb returns.
func (f *fakeTracker) Inbox(_ context.Context, q worklet.InboxQuery,
	_ time.Time) (worklet.InboxAnswer, error) {

	f.inboxQuery = q
	if f.inbox != nil {
		return *f.inbox, nil
	}
	return worklet.InboxAnswer{
		Handle:         q.Handle,
		PrimaryReasons: worklet.DefaultPrimaryReasons,
		Notices: []worklet.InboxNotice{{
			RecordID: "rec-1", SubjectID: "i1", SubjectKey: "ENG-1",
			Reason: worklet.ReasonAssignee, Primary: true,
		}},
		Primary: 1, Unread: 1,
	}, nil
}

// Depend records the dependency change the tool composed.
//
// THE COMPOSITION IS THE HALF THAT CAN BE WRONG HERE: the tool turns keys into
// ids and a `{set}` gesture into the adds and removes a two-ended write needs,
// and the sequence itself is certified against a real store elsewhere.
func (f *fakeTracker) Depend(_ context.Context, _ string,
	change worklet.DependencyChange, _ worklet.Leads) (worklet.DependencyResult, error) {

	f.depended = append(f.depended, change)
	if f.dependErr != nil {
		return worklet.DependencyResult{}, f.dependErr
	}
	if f.dependAnswer != nil {
		return *f.dependAnswer, nil
	}
	return worklet.DependencyResult{WriteResult: worklet.WriteResult{
		Result: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: 41},
		},
	}}, nil
}

// EVERY FILTER THIS TOOL DECLARES REACHES THE QUERY. The board's grammar
// compiles forty-odd indexed keys, reachable from the dashboard and the REST
// route, and a seat needs them too: the type, a due window, what moved since,
// the subtasks of an item, what it filed or follows, a page after the first,
// and the company's CUSTOM FIELDS.
func TestEveryDeclaredFilterReachesTheQuery(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	if got := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{
		"type": "bug", "priority": "high", "due": "thisweek",
		"updated": "gte:-1d", "created": "gte:-7d", "parent": "ENG-1",
		"reporter": "bo", "watcher": "ana", "unit": "platform",
		"sort": "-updated", "cursor": "c1",
		"field_filters": map[string]any{"impact": "high"},
	}); got.Failed {
		t.Fatalf("the filtered list failed: %s", got.Output)
	}
	for key, want := range map[string]string{
		"type": "bug", "priority": "high", "due": "thisweek",
		"updated": "gte:-1d", "created": "gte:-7d",
		// RESOLVED, not forwarded: `parent` is matched against the id
		// column. See TestTheParentFilterIsResolvedToAnID.
		"parent":   "i1",
		"reporter": "bo", "watcher": "ana", "unit": "platform",
		"sort": "-updated", "cursor": "c1", "f.impact": "high",
	} {
		if got := trk.params[key]; got != want {
			t.Errorf("the query carries %s=%q, want %q — a filter this tool "+
				"declares and does not forward is one a model asks for, is "+
				"not refused for, and gets an unfiltered list back from",
				key, got, want)
		}
	}
}

// A PAGING ARGUMENT NEEDS A PAGE TO COME FROM. `cursor` asks for "the
// `next_cursor` from a previous call", so the answer carries one, as the REST
// route's does: an argument whose only source is an answer the tool does not
// give is one nobody can use.
func TestTheListAnswerCarriesTheCursorItsOwnArgumentAsksFor(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.answer = &worklet.Answer{
		Complete:   true,
		Rows:       []worklet.TaskRow{{ID: "i1", Key: "ENG-1", Title: "one"}},
		NextCursor: "opaque-cursor",
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{})
	if got.Failed {
		t.Fatalf("the list failed: %s", got.Output)
	}
	if !strings.Contains(got.Output, "opaque-cursor") {
		t.Fatalf("the answer is %q and carries no next_cursor, so the "+
			"`cursor` argument beside it can never be used", got.Output)
	}
}

// A REFERENCE ARGUMENT IS RESOLVED TO AN ID, and `parent` is matched against
// `parent_id`, which holds one. A model types the key it read, and forwarded
// raw, `parent: ENG-1` compares a key against an id column and answers an
// EMPTY LIST with no error — "no subtasks" rather than a refusal, so nobody
// finds out (the failure `references` describes in the parser).
func TestTheParentFilterIsResolvedToAnID(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	if got := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{
		"parent": "ENG-1",
	}); got.Failed {
		t.Fatalf("the list failed: %s", got.Output)
	}
	if got := trk.params["parent"]; got != "i1" {
		t.Fatalf("the query filters on parent=%q — a key compared against the "+
			"id column answers an empty list and no error, which a model "+
			"reads as the item having no subtasks", got)
	}
}

// A PATCH WHOSE ONLY ARGUMENT IS A CUSTOM FIELD OR A RE-ROUTE IS A WRITE. The
// emptiness check decides whether the write is issued at all, so a field it
// does not see answers `outcome: applied` and changes nothing — the one
// failure a seat cannot detect, with no error to read and no history row
// missing.
func TestAFieldsOnlyUpdateIsNotSilentlyDropped(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"a custom field": {"item": "ENG-1", "fields": map[string]any{"severity": "sev2"}},
		"a re-route":     {"item": "ENG-1", "routing_unit": "ops"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			// THE RE-ROUTE IS THE LEAD'S DECISION, so the registry
			// carries one — otherwise that case would be refused for
			// a reason unrelated to what it is asserting.
			reg := projectRegistry(t, trk, chartLeads)
			got := callWork(t, reg, builtin.UpdateWorkItemTool, args)
			if got.Failed {
				t.Fatalf("the update failed: %s", got.Output)
			}
			if len(trk.patched) == 0 {
				t.Fatalf("%s reached no write at all, and the tool answered "+
					"%q — a seat is told the change landed and the item is "+
					"unchanged on every node", name, got.Output)
			}
		})
	}
}

// OPENING ONE COMMENT IS A READ A MODEL CAN REACH, AND A WRONG ID SAYS SO. The
// thread page carries EXCERPTS (twenty whole bodies are ten times the ceiling
// on one tool answer), which is honest only while the rest is one call away:
// these cases are the `comment` argument and the refusal that make an excerpt
// past 2 KiB a pointer rather than a loss.
func TestOneCommentCanBeOpenedWholeAndAWrongIDSaysHow(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk})

	if got := callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "comment": "cm-9",
	}); got.Failed {
		t.Fatalf("opening one comment failed: %q", got.Output)
	}
	if trk.wants.Comment != "cm-9" {
		t.Fatalf("the read was given Comment %q — the argument never reached "+
			"the reader, so the excerpt has no way back", trk.wants.Comment)
	}
	// AND IT NARROWS THE ANSWER ON ITS OWN. A whole item is already over
	// the ceiling at its maximum, so one whole comment carried beside the
	// history, the links and the fields would meet the refusal that tells
	// a caller to narrow — from the read that exists to recover a body.
	if trk.wants.History || trk.wants.Links || trk.wants.Fields {
		t.Errorf("opening one comment also asked for history=%v links=%v "+
			"fields=%v, which can push the answer past the ceiling",
			trk.wants.History, trk.wants.Links, trk.wants.Fields)
	}
	// AN EXPLICIT `include` STILL WINS: a caller asking for both means it.
	callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "comment": "cm-9", "include": []any{"fields"},
	})
	if !trk.wants.Fields || trk.wants.Comment != "cm-9" {
		t.Errorf("an explicit include beside `comment` gave fields=%v "+
			"comment=%q", trk.wants.Fields, trk.wants.Comment)
	}

	// AND IT IS ITS OWN REFUSAL. A mistyped comment id is not an empty
	// thread, and a reader told "no comments" goes looking for the wrong
	// thing — so the message names where ids come from and how to get the
	// thread back.
	trk.readErr = fmt.Errorf("%w: cm-nope on i1", worklet.ErrNoComment)
	got := callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "comment": "cm-nope",
	})
	if !got.Failed {
		t.Fatal("a comment that is not on the item read back as a result")
	}
	for _, want := range []string{"cm-nope", "comment"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal is %q and does not name %q — a refusal a "+
				"caller cannot act on is one they guess against", got.Output, want)
		}
	}
}

// THE DESCRIPTION IS WHOLE OR ABSENT, NEVER CUT: a model acts on the opening
// of a cut spec as the spec. A default read carries it whole; a read naming
// other parts carries none of it and SAYS so with its size, so an item with a
// description never reads as one without.
func TestADescriptionIsWholeOrSaidToBeLeftOut(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	whole := strings.Repeat("d", worklet.MaxBody)
	item := trk.tasks["ENG-1"]
	item.Task.Body = whole
	trk.tasks["ENG-1"] = item
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk})

	got := callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "include": []any{"body"},
	})
	if got.Failed {
		t.Fatalf("reading the description failed: %q", got.Output)
	}
	if !strings.Contains(got.Output, whole) {
		t.Error("the description at its cap did not come back whole")
	}
	if strings.Contains(got.Output, "…") {
		t.Error("a whole description was marked as cut")
	}

	got = callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "include": []any{"history"},
	})
	if got.Failed {
		t.Fatalf("a read without the description failed: %q", got.Output)
	}
	if strings.Contains(got.Output, "ddd") {
		t.Error("a read that did not ask for the description carried some of it")
	}
	if !strings.Contains(got.Output, "body_not_included") ||
		!strings.Contains(got.Output, strconv.Itoa(len(whole))+" bytes") {
		t.Errorf("a read without the description did not say it left one out: %.300s", got.Output)
	}

	// `comment` ON ITS OWN is that comment, not the description beside it.
	got = callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1", "comment": "cm-9"})
	if !got.Failed && strings.Contains(got.Output, whole) {
		t.Error("naming one comment also carried the whole description")
	}
}

// WHEN A TASK IS DUE AND HOW BIG IT IS. The query grammar filters on all four
// columns, a row's `overdue` flag derives from the due date and every total
// sums the sizing pair, so a tool that could not set them would leave all of
// that dead surface that looks like an empty company.
func TestACreateSetsWhenAndHowBig(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "ship the planner", "project": "ENG",
		"due": "2031-04-16", "start": "2031-04-01",
		"estimate_minutes": 240, "points": 8,
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	if len(trk.created) != 1 {
		t.Fatalf("created %d tasks, want 1", len(trk.created))
	}
	task := trk.created[0]
	if task.DueAt == nil || task.DueAt.Format("2006-01-02") != "2031-04-16" {
		t.Errorf("due is %v, want 2031-04-16", task.DueAt)
	}
	// A TOKEN THAT NAMED A DAY SETS THE ALL-DAY FLAG, so a renderer shows
	// "16 April" rather than "16 April, 00:00" for a date nobody timed.
	if !task.DueAllDay {
		t.Error("a date with no time on it did not set the all-day flag")
	}
	if task.StartAt == nil || task.StartAt.Format("2006-01-02") != "2031-04-01" {
		t.Errorf("start is %v, want 2031-04-01", task.StartAt)
	}
	if task.EstimateMinutes != 240 || task.Points != 8 {
		t.Errorf("sizing is %d minutes / %v points, want 240 and 8",
			task.EstimateMinutes, task.Points)
	}
}

// A TASK IS FILED INTO THE LANE IT WAS FILED FROM. A board lane's "+" names
// the lane's status, and a create that could only land in `todo` would make
// that two writes, the second free to fail after the first filed the task in
// the wrong column. The group travels with the status, and an unknown status
// is refused by name rather than filed as `todo`.
func TestACreateStartsInTheStatusItNames(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "already under way", "project": "ENG", "status": "in_progress",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	task := trk.created[0]
	if task.Status != worklet.StatusInProgress || task.StatusGroup != worklet.GroupActive {
		t.Errorf("filed as %s/%s, want in_progress/active", task.Status, task.StatusGroup)
	}

	plain := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "not started", "project": "ENG",
	})
	if plain.Failed || trk.created[1].Status != worklet.StatusTodo {
		t.Errorf("a create naming no status filed %q (%s), want todo",
			trk.created[1].Status, plain.Output)
	}

	bad := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "somewhere", "project": "ENG", "status": "shipping",
	})
	if !bad.Failed || !strings.Contains(bad.Output, `"shipping" is not a status. The statuses are: `+
		words.And(worklet.Statuses)+".") {
		t.Errorf("an unknown status was not refused by name, naming every status: %s", bad.Output)
	}
	if len(trk.created) != 2 {
		t.Errorf("the refused create filed a task anyway (%d filed)", len(trk.created))
	}
}

// WHO A CREATE WAS FILED TO IS THE TRACKER'S ANSWER, never the argument. A
// create naming nobody goes to the project's default assignee, which only the
// create's own snapshot decides, so echoing the empty argument would tell a
// model, and a person's surface, the work went to triage while it sat on a
// colleague's queue.
func TestACreatesAnswerNamesWhoTheTrackerFiledItTo(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.defaultAssignee = "bo"
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "nobody named", "project": "ENG",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	var answer struct {
		Assignee string `json:"assignee"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("decode the answer: %v (%s)", err, got.Output)
	}
	if trk.created[0].Assignee != "" {
		t.Fatalf("the tool named %q itself; the default is the tracker's to settle",
			trk.created[0].Assignee)
	}
	if answer.Assignee != "bo" {
		t.Errorf("the answer names %q as the assignee, want the tracker's %q",
			answer.Assignee, "bo")
	}
}

// THE SAME GRAMMAR THE FILTER READS. A model that can ask for everything due
// this week can say "due this week" about one task, and a second spelling here
// would be the copy that stops matching.
func TestADueDateTakesTheRelativeWordsTheFilterTakes(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "soon", "project": "ENG", "due": "+3d",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	if trk.created[0].DueAt == nil {
		t.Fatal("`+3d` resolved to no due date at all")
	}
	// AND A TOKEN NOTHING CAN READ IS REFUSED BY NAME rather than dropped:
	// a write that succeeds while silently setting no date is the one a
	// model reads as having set one.
	bad := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "whenever", "project": "ENG", "due": "next tuesday-ish",
	})
	if !bad.Failed {
		t.Error("an unreadable due date was accepted, so the task was filed " +
			"with no date and the answer said it worked")
	}
}

// AN UPDATE MOVES THEM, AND `null` TAKES ONE BACK OFF. "Leave the due date
// alone" and "this has no due date any more" are different edits, and a tool
// that could only say the first makes a date impossible to remove.
//
// Mutation: clear the due date in applyToPatch with a pointer at the zero
// instant and the patch carries no flag.
func TestAnUpdateSetsAndClearsTheSchedule(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "due": "2031-05-02", "points": 13,
	}); got.Failed {
		t.Fatalf("update failed: %s", got.Output)
	}
	patch := trk.patched[len(trk.patched)-1]
	if patch.DueAt == nil || patch.DueAt.Format("2006-01-02") != "2031-05-02" {
		t.Errorf("the patch's due date is %v, want 2031-05-02", patch.DueAt)
	}
	if patch.Points == nil || *patch.Points != 13 {
		t.Errorf("the patch's points are %v, want 13", patch.Points)
	}

	if got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "due": nil,
	}); got.Failed {
		t.Fatalf("clearing failed: %s", got.Output)
	}
	cleared := trk.patched[len(trk.patched)-1]
	// A CLEAR IS ITS FLAG, not an absent field — nil on a patch means "not
	// named" — and never the zero instant, which is no date (the patch type's
	// doc says why).
	if !cleared.ClearDue || cleared.DueAt != nil || cleared.ClearStart {
		t.Errorf("a cleared due date is (clear_due %v, due_at %v, clear_start %v), want the "+
			"due date's flag alone", cleared.ClearDue, cleared.DueAt, cleared.ClearStart)
	}
	if err := cleared.CheckDates(); err != nil {
		t.Errorf("the tracker refuses the clear the tool formed: %v", err)
	}
}

// A SIZE IS NOT NEGATIVE, and one is refused naming the rule rather than
// stored — a negative estimate would be subtracted from its own board's
// total. Nor is a date the zero instant, which is no date at all.
//
// Mutation: drop readSchedule's refusal of the zero instant and the create due
// in year one is accepted.
func TestTheSchedulingValuesAreRefusedRatherThanStoredWrong(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	for _, args := range []map[string]any{
		{"title": "x", "project": "ENG", "points": -3},
		{"title": "x", "project": "ENG", "estimate_minutes": -60},
		// THE ZERO INSTANT IS NO DATE: stored, it is the most overdue task
		// the company ever had, and the tracker refuses it in words about
		// its own fields rather than this tool's.
		{"title": "x", "project": "ENG", "due": "0001-01-01T00:00:00Z"},
	} {
		if got := callWork(t, reg, builtin.CreateWorkItemTool, args); !got.Failed {
			t.Errorf("create(%v) was accepted, want a refusal naming the rule", args)
		}
	}
}

// AN UNREADABLE SIZE IS REFUSED, NOT READ AS ZERO: the date rule above,
// applied to the rest of `readSchedule`. Each field is a POINTER because its
// zero is a setting (zero minutes or points means UNESTIMATED), so an
// unreadable value read as `&0` would answer `applied` while WIPING the
// estimate. `"2 days"` guards against a reader that takes the leading integer
// (`fmt.Sscanf("%d")`) and stores a two-day estimate as two MINUTES.
func TestAnUnreadableSizeIsRefusedRatherThanReadAsZero(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"an estimate in words", "estimate_minutes", "two hours"},
		{"an estimate with a unit", "estimate_minutes", "2 days"},
		{"a fractional minute", "estimate_minutes", 1.5},
		{"points in words", "points", "five"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

			got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
				"title": "sized wrong", "project": "ENG", tc.field: tc.value,
			})
			if !got.Failed {
				t.Fatalf("%s=%v was accepted; the task was filed as "+
					"UNESTIMATED and the answer said it worked", tc.field, tc.value)
			}
			// AND THE REFUSAL NAMES THE FIELD, which is the whole of the
			// repair: a model cannot fix what it is not told about.
			if !strings.Contains(got.Output, tc.field) {
				t.Errorf("the refusal does not name `%s`: %s", tc.field, got.Output)
			}
		})
	}
}

// AND A NUMBER THAT READS IS STILL TAKEN, so the guard above refuses the
// unreadable rather than the unfamiliar. A JSON number, a whole float and a
// numeric string are all the same estimate.
func TestAReadableSizeIsStillTakenInEverySpelling(t *testing.T) {
	t.Parallel()
	for _, value := range []any{90, 90.0, "90"} {
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

		got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
			"title": "sized", "project": "ENG", "estimate_minutes": value,
		})
		if got.Failed {
			t.Fatalf("estimate_minutes=%v (%T) was refused: %s", value, value, got.Output)
		}
		if trk.created[0].EstimateMinutes != 90 {
			t.Errorf("estimate_minutes=%v (%T) stored %d, want 90",
				value, value, trk.created[0].EstimateMinutes)
		}
	}
}

// AND ZERO IS STILL A VALUE somebody can set, which is what distinguishes this
// from refusing falsy input: "this takes no time" is a statement, and the
// pointer is what carries it.
func TestAnExplicitZeroEstimateIsSet(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "free", "project": "ENG", "estimate_minutes": 0,
	})
	if got.Failed {
		t.Fatalf("an explicit zero estimate was refused: %s", got.Output)
	}
}

// A SIZE THAT IS NOT A NUMBER IS REFUSED, and NaN gets past a range check by
// definition: every comparison with it is false, so `points < 0` says nothing
// about it, and an infinity passes the same guard. Neither can be summed into
// a total nor encoded in JSON, so the failure would surface downstream with no
// memory of who typed it.
func TestANonFiniteSizeIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"points as NaN", "points", "NaN"},
		{"points as an infinity", "points", "Inf"},
		{"points as a spelled-out infinity", "points", "infinity"},
		{"points as a negative infinity", "points", "-Inf"},
		// `int(+Inf)` is undefined by the language and lands on the minimum int,
		// which the negative check would refuse as NEGATIVE: the right answer for
		// the wrong reason, naming a sign nobody typed.
		{"an infinite estimate", "estimate_minutes", math.Inf(1)},
		{"a NaN estimate", "estimate_minutes", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

			got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
				"title": "sized wrong", "project": "ENG", tc.field: tc.value,
			})
			if !got.Failed {
				t.Fatalf("%s=%v was accepted and reached the writer", tc.field, tc.value)
			}
			if !strings.Contains(got.Output, tc.field) {
				t.Errorf("the refusal does not name `%s`: %s", tc.field, got.Output)
			}
		})
	}
}

// THE UPDATE SCHEMA ADMITS THE NULL ITS OWN DESCRIPTION PROMISES. Clearing a
// date or a size is done by passing null, and a caller that VALIDATES against
// a schema declaring `string` or `integer` alone refuses the null before the
// tool is reached.
//
// A CREATE STAYS NON-NULLABLE, because a create has nothing to clear: a null
// there is a value nobody meant rather than an instruction.
func TestOnlyTheUpdateSchemaAcceptsANullClear(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	typeOf := func(tool, field string) any {
		t.Helper()
		entry, held := reg.Lookup(tool)
		if !held {
			t.Fatalf("no %s tool", tool)
		}
		props, _ := entry.Tool.Parameters()["properties"].(map[string]any)
		prop, _ := props[field].(map[string]any)
		return prop["type"]
	}

	for _, field := range []string{"due", "start", "estimate_minutes", "points"} {
		update := typeOf(builtin.UpdateWorkItemTool, field)
		types, ok := update.([]string)
		if !ok {
			t.Errorf("update's %q is %v (%T), want a union admitting null",
				field, update, update)
			continue
		}
		if !slices.Contains(types, "null") {
			t.Errorf("update's %q is %v and cannot express the clear its own "+
				"description documents", field, types)
		}
		// AND THE VALUE TYPE SURVIVES the union: admitting null must not
		// stop the field accepting what it is for.
		if len(types) != 2 || types[1] != "null" {
			t.Errorf("update's %q is %v, want its own type then null", field, types)
		}

		if create := typeOf(builtin.CreateWorkItemTool, field); create == nil {
			t.Errorf("create's %q declares no type at all", field)
		} else if _, union := create.([]string); union {
			t.Errorf("create's %q is %v; a create has nothing to clear", field, create)
		}
	}
}

// A SCHEDULE EDIT REACHES THE WAKE IT ANNOUNCES. The notification's deltas
// (`TaskDeltas`) compare the task this tool READ with a snapshot of it with
// the patch applied, so a snapshot that missed the schedule fields would
// announce every date, estimate and size a seat moved as a change that
// changed nothing.
//
// The snapshot is the writer's merge, so this asserts the CONSEQUENCE rather
// than the copy: a test over the field list would pass again the day somebody
// adds a field to the patch and forgets it.
func TestASceduleEditReachesTheNotification(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	seed := trk.tasks["ENG-1"]
	seed.Task.DueAt = nil
	seed.Task.EstimateMinutes = 30
	trk.tasks["ENG-1"] = seed
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "due": "2031-04-16", "estimate_minutes": 90,
	})
	if got.Failed {
		t.Fatalf("update failed: %s", got.Output)
	}
	notify := trk.notified[len(trk.notified)-1]
	if notify == nil {
		t.Fatal("the write carried no notification at all")
	}
	for _, field := range []string{"due", "estimate"} {
		delta, held := notify.Fields[field]
		if !held {
			t.Errorf("the wake carries no %q delta, so the card announces a "+
				"change that changed nothing (fields: %v)", field, notify.Fields)
			continue
		}
		if delta.To == delta.From {
			t.Errorf("%s moved from %q to %q — the snapshot was compared "+
				"against itself", field, delta.From, delta.To)
		}
	}
	if got := notify.Fields["estimate"]; got.From != "30m" || got.To != "90m" {
		t.Errorf("estimate delta = %+v, want 30m → 90m", got)
	}
}

// A CREATE ASKED TWICE UNDER ONE SEED FILES ONE ITEM. The operation a create
// is published under names the new item's id, so an id minted fresh per call
// makes every retry a fresh OPERATION the ledger never recognises: a
// redelivered turn, or an HTTP create retried under its Idempotency-Key after
// an `unknown`, files its item twice.
//
// The controls are the two things one seed must still tell apart: a
// different item asked for in the same run, and a call with no seed at all.
func TestACreateRetriedUnderOneSeedFilesOnce(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	create := func(title string) {
		t.Helper()
		if got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
			"title": title, "project": "ENG",
		}); got.Failed {
			t.Fatalf("create %q: %s", title, got.Output)
		}
	}
	create("rotate the signing key")
	create("rotate the signing key")
	create("audit the access log")
	if len(trk.created) != 3 {
		t.Fatalf("filed %d creates, want three calls recorded", len(trk.created))
	}
	if trk.created[0].ID != trk.created[1].ID || trk.opIDs[0] != trk.opIDs[1] {
		t.Errorf("one item asked for twice under one seed was two operations: "+
			"%s/%s and %s/%s", trk.created[0].ID, trk.opIDs[0],
			trk.created[1].ID, trk.opIDs[1])
	}
	if trk.created[2].ID == trk.created[0].ID {
		t.Error("two different items in one run were given one id")
	}

	// AND WITH NO SEED there is nothing to be idempotent against, so two
	// calls are two items — an operator filing the same title twice meant
	// it twice.
	plain := newFakeTracker()
	operator := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{Handle: "token:ops", Kind: worklet.AuthorOperator}, nil
	}
	tools := builtin.OperatorTools(builtin.OperatorDeps{
		Work:      builtin.WorkDeps{Reader: plain, Writer: plain.as, Actor: operator},
		Authorize: builtin.Decide(chartLeads),
	})
	for _, tool := range tools {
		if tool.Name() != builtin.CreateWorkItemTool {
			continue
		}
		for range 2 {
			got, err := tool.Call(everyGrant(), map[string]any{
				"title": "rotate the signing key", "project": "ENG",
			})
			if err != nil || got.Failed {
				t.Fatalf("operator create: %v %s", err, got.Output)
			}
		}
	}
	if len(plain.created) != 2 || plain.created[0].ID == plain.created[1].ID {
		t.Errorf("two unseeded creates were given one id: %+v", plain.created)
	}
}

// A CREATE THAT NAMES NO PROJECT IS ONE OPERATION WHEREVER ITS DEFAULT POINTS.
// The default is read from the chart when the call runs, so it can move
// between a turn and its re-run (the seat changed team, a subtask's parent
// moved project), and an operation named for the resolved project would file
// a second item. The call is the operation; a project it NAMES stays part of
// it, as the control shows.
func TestACreateRerunAfterItsDefaultMovedFilesOnce(t *testing.T) {
	t.Parallel()
	file := func(home string, args map[string]any) (string, string) {
		t.Helper()
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			DefaultProject: func(string) string { return home },
		})
		if got := callWork(t, reg, builtin.CreateWorkItemTool, args); got.Failed {
			t.Fatalf("create in %s: %s", home, got.Output)
		}
		if len(trk.created) != 1 || trk.created[0].Project != home &&
			args["project"] == nil {

			t.Fatalf("the create in %s filed %+v", home, trk.created)
		}
		return trk.created[0].ID, trk.opIDs[0]
	}
	unnamed := map[string]any{"title": "rotate the signing key"}
	firstID, firstOp := file("ENG", unnamed)
	rerunID, rerunOp := file("OPS", unnamed)
	if firstID != rerunID || firstOp != rerunOp {
		t.Errorf("a re-run whose default moved was another operation: %s/%s "+
			"then %s/%s — a second item for one request",
			firstID, firstOp, rerunID, rerunOp)
	}

	// THE CONTROL: a project the call NAMES is part of the call.
	engID, _ := file("ENG", map[string]any{"title": "rotate the signing key", "project": "ENG"})
	opsID, _ := file("ENG", map[string]any{"title": "rotate the signing key", "project": "OPS"})
	if engID == opsID {
		t.Error("two creates naming two projects were given one id")
	}
}

// declaredOf is args cut to the ones tool declares, for a case that calls
// every tool with one set of arguments and is about something other than
// what an undeclared one is answered — which the gate refuses by name before
// anything else is decided.
func declaredOf(tool tools.Callable, args map[string]any) map[string]any {
	properties, _ := tool.Parameters()["properties"].(map[string]any)
	out := map[string]any{}
	for name, value := range args {
		if _, held := properties[name]; held {
			out[name] = value
		}
	}
	return out
}
