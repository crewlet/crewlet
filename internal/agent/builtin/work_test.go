package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ---- the fakes -------------------------------------------------------- //

type fakeTracker struct {
	// answer overrides what Tasks returns, for the cases that are about
	// the SHAPE of an answer rather than about the rows in it.
	answer *tracker.Answer

	// query is the last one the list tool built, so a case can assert
	// what a model's ARGUMENTS became — which is the half of this tool
	// nothing looked at while it dropped a filter and refused a flag.
	query tracker.Query

	// params is what the tool handed the grammar, before it was parsed.
	params map[string]string
	tasks  map[string]tracker.TaskDetail

	// reads is every freshness the point readers were handed.
	reads []statelog.Freshness

	// wants is the last DetailWants a detail read was given, so a case can
	// assert what a model's arguments BECAME rather than only that the call
	// came back.
	wants tracker.DetailWants

	created []tracker.Task
	merged  []mergeCall

	// searched is every text the ranked search was asked for, and ranked
	// what it answers with.
	searched  []string
	ranked    []tracker.Ranked
	searchErr error
	patched   []tracker.TaskPatch
	ifMatch   []uint64
	notified  []*tracker.Notify

	// kinds is what each write said it WAS, which is a different fact
	// from whether it notified anybody — see [tracker.MutationRecord.Kind].
	kinds  []tracker.ChangeKind
	actors []builtin.Actor
	opIDs  []string

	// thread is what Thread answers and threadQuery the last one asked,
	// which is how a case asserts what the comment tool RESOLVED rather
	// than only what it wrote.
	thread      tracker.ResolvedThread
	threadQuery tracker.ThreadQuery
	threadErr   error

	// depended is every dependency change the tool composed, and
	// dependErr what the sequence answers.
	depended  []tracker.DependencyChange
	dependErr error

	projectEdits     []tracker.ProjectEdit
	projectAuthority []tracker.ProjectAuthority

	// declaredTypes and declaredFields are the catalogue as the WRITE
	// tools composed it, which is the half of those verbs that lives in
	// this package: the tracker's own suite certifies what it does with a
	// declaration, and nothing else can say what a model's arguments
	// BECAME.
	declaredTypes  [][]tracker.TaskType
	declaredFields [][]tracker.FieldDef
	tagEdits       []tracker.TagEdit
	tagAuthority   []tracker.TagAuthority
	tagWarnings    []string
	ensured        [][]string
	ensuredIn      []string

	projectQuery tracker.ProjectQuery
	projects     tracker.ProjectListing
	viewQuery    tracker.ViewQuery
	detailQuery  tracker.ProjectDetailQuery
	project      tracker.ProjectDetail

	activityQuery tracker.ActivityQuery
	activity      tracker.ActivityAnswer
	myWorkQuery   tracker.MyWorkQuery
	myWork        tracker.MyWork

	// viewer is who the last list was expanded FOR, which is the half of a
	// personal filter — `preset=my_queue`, `assignee=me` — that decides
	// whether a founder's assistant is answered about them or about the
	// credential in their hand.
	viewer tracker.Viewer

	// personQuery and inboxQuery are the last personal reads this fake was
	// asked, so a case can assert WHO a tool resolved the question to —
	// which is the half of these two verbs that decides whether a founder
	// is shown their own day or a credential's.
	personQuery tracker.PersonQuery
	inboxQuery  tracker.InboxQuery

	readErr  error
	writeErr error
}

func newFakeTracker() *fakeTracker {
	tasks := map[string]tracker.TaskDetail{
		"ENG-1": {
			Task: tracker.Task{
				ID: "i1", Key: "ENG-1", Project: "ENG", Title: "the work",
				Status: tracker.StatusTodo, StatusGroup: tracker.GroupNotStarted,
				Version: 7,
			},
			Complete: true,
		},
	}
	// THREE MORE TO POINT AT, because every relation argument resolves its
	// references to IDS before anything is written — a key stored in a
	// relation resolves to nothing on every node, for ever.
	for _, n := range []string{"2", "3", "4"} {
		tasks["ENG-"+n] = tracker.TaskDetail{
			Task: tracker.Task{
				ID: "id-" + n, Key: "ENG-" + n, Project: "ENG",
				Status: tracker.StatusTodo, StatusGroup: tracker.GroupNotStarted,
			},
			Complete: true,
		}
	}
	return &fakeTracker{tasks: tasks}
}

func (f *fakeTracker) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	f.query = q
	if f.readErr != nil {
		return tracker.Answer{}, f.readErr
	}
	if f.answer != nil {
		return *f.answer, nil
	}
	answer := tracker.Answer{Complete: true, Level: statelog.ReadSession}
	for _, d := range f.tasks {
		answer.Rows = append(answer.Rows, tracker.TaskRow{
			ID: d.Task.ID, Key: d.Task.Key, Title: d.Task.Title,
			Status: d.Task.Status, Project: d.Task.Project,
		})
	}
	answer.TotalHint = len(answer.Rows)
	return answer, nil
}

func (f *fakeTracker) Task(_ context.Context, idOrKey string, want tracker.DetailWants,
	fresh statelog.Freshness) (tracker.TaskDetail, error) {

	f.reads = append(f.reads, fresh)
	f.wants = want
	if f.readErr != nil {
		return tracker.TaskDetail{}, f.readErr
	}
	for key, d := range f.tasks {
		if key == strings.ToUpper(idOrKey) || d.Task.ID == idOrKey {
			return d, nil
		}
	}
	return tracker.TaskDetail{}, fmt.Errorf("%w: %s", tracker.ErrNoTask, idOrKey)
}

func (f *fakeTracker) Views(_ context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	f.viewQuery = q
	return tracker.ViewListing{}, nil
}

func (f *fakeTracker) ExpandedQuery(_ context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {

	f.viewer = viewer

	// THE KEYS THE TOOL COMPOSED, kept as strings: what a case here is
	// about is the TRANSLATION from a model's arguments to the grammar's
	// own keys, and the parse below is the real one, so a key this tool
	// never forwarded reaches neither.
	f.params = map[string]string{}
	for key, value := range params {
		f.params[key] = fmt.Sprint(value)
	}
	return tracker.ParseQuery(tracker.MapParams(params), now, loc)
}

func (f *fakeTracker) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (f *fakeTracker) Person(_ context.Context, q tracker.PersonQuery,
	_ time.Time) (tracker.PersonState, error) {

	f.personQuery = q
	return tracker.PersonState{Handle: q.Who.Handle}, nil
}

// Thread answers what the fake was TOLD to answer, and records the query.
//
// A REAL SEAM RATHER THAN A ZERO VALUE: the comment tool's ask handling is
// decided by what comes back from here — who is in the thread, which open ask
// was inferred, whose question is being answered — so a stub returning nothing
// would let every one of those arms pass while doing nothing at all.
func (f *fakeTracker) Thread(_ context.Context, q tracker.ThreadQuery,
	fresh statelog.Freshness) (tracker.ResolvedThread, error) {

	f.reads = append(f.reads, fresh)

	f.threadQuery = q
	if f.threadErr != nil {
		return tracker.ResolvedThread{}, f.threadErr
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

// as records the actor and hands back a writer bound to it, which is the
// tracker's own rule: a writer acts as exactly one party.
// The PROJECT seam, which [builtin.ProjectReader] asserts for: a reader that
// answers the task questions and not these is a build with no native tracker,
// and the registration turns on exactly that.
func (f *fakeTracker) Projects(_ context.Context, q tracker.ProjectQuery) (
	tracker.ProjectListing, error) {

	f.projectQuery = q
	return f.projects, f.readErr
}

func (f *fakeTracker) Project(_ context.Context, q tracker.ProjectDetailQuery) (
	tracker.ProjectDetail, error) {

	f.detailQuery = q
	return f.project, f.readErr
}

// The FEED seam — what happened, and what is waiting on somebody. A reader
// that answers the task questions and not these is a build with no native
// tracker, and the registration turns on exactly that.
func (f *fakeTracker) Activity(_ context.Context, q tracker.ActivityQuery,
	_ time.Time) (tracker.ActivityAnswer, error) {

	f.activityQuery = q
	return f.activity, f.readErr
}

func (f *fakeTracker) MyWork(_ context.Context, q tracker.MyWorkQuery,
	_ time.Time) (tracker.MyWork, error) {

	f.myWorkQuery = q
	return f.myWork, f.readErr
}

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
func (f *fakeTracker) Search(_ context.Context, text string,
	limit int) ([]tracker.Ranked, error) {

	f.searched = append(f.searched, text)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if limit <= 0 || limit > len(f.ranked) {
		return f.ranked, nil
	}
	return f.ranked[:limit], nil
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
	notify          *tracker.Notify
}

// MergeDuplicates records the fold, for the same reason Depend does: what the
// TOOL resolves and decides is the half that lives here, and the sequence
// itself is certified against a real store in the tracker's own suite.
func (f *fakeTracker) MergeDuplicates(_ context.Context, _ string,
	duplicate, into string, reparent bool,
	notify *tracker.Notify) (tracker.WriteResult, error) {

	f.merged = append(f.merged, mergeCall{
		duplicate: duplicate, into: into, reparent: reparent, notify: notify,
	})
	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	return tracker.WriteResult{Result: statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 51},
		Version:  51,
	}}, nil
}

func (f *fakeTracker) CreateTask(_ context.Context, opID string, task tracker.Task,
	notify *tracker.Notify) (tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.created = append(f.created, task)
	f.notified = append(f.notified, notify)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Key: "ENG-9", Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 11},
		Version:  11,
	}, nil
}

func (f *fakeTracker) UpdateTask(_ context.Context, opID, _, _ string, ifMatch uint64,
	patch tracker.TaskPatch, kind tracker.ChangeKind,
	notify *tracker.Notify) (tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.patched = append(f.patched, patch)
	f.ifMatch = append(f.ifMatch, ifMatch)
	f.notified = append(f.notified, notify)
	f.kinds = append(f.kinds, kind)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Key: "ENG-1", Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 12},
		Version:  12,
	}, nil
}

// The PROJECT write side, which every surface has: declaring a tag is open to
// every seat, so a seat without it could never use the `labels` argument on
// the create and update tools it already holds.
func (f *fakeTracker) WriteProject(_ context.Context, opID, key string,
	edit tracker.ProjectEdit, authority tracker.ProjectAuthority) (
	tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.projectEdits = append(f.projectEdits, edit)
	f.projectAuthority = append(f.projectAuthority, authority)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 3,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 13},
	}, nil
}

func (f *fakeTracker) WriteTags(_ context.Context, opID, project string,
	edit tracker.TagEdit, authority tracker.TagAuthority) (
	tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.tagEdits = append(f.tagEdits, edit)
	f.tagAuthority = append(f.tagAuthority, authority)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 4,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 14},
		Warnings: f.tagWarnings,
	}, nil
}

// The CATALOGUE write side, which is the OPERATOR's alone. The declaration a
// tool composed is what a case here asserts; whether the tracker accepts it is
// the tracker's own suite (internal/tracker/config_test.go), and a second copy
// of that gate in this fake would certify nothing but itself.
func (f *fakeTracker) WriteTypes(_ context.Context, opID string,
	types []tracker.TaskType) (tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.declaredTypes = append(f.declaredTypes, types)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 5,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 15},
	}, nil
}

func (f *fakeTracker) WriteFields(_ context.Context, opID string,
	fields []tracker.FieldDef) (tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.declaredFields = append(f.declaredFields, fields)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Outcome: statelog.OutcomeApplied, Version: 6,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 16},
	}, nil
}

func (f *fakeTracker) EnsureTags(_ context.Context, opID, project string,
	tags []string) ([]string, []string, error) {

	if f.writeErr != nil {
		return nil, nil, f.writeErr
	}
	f.ensured = append(f.ensured, tags)
	f.ensuredIn = append(f.ensuredIn, project)
	f.opIDs = append(f.opIDs, opID)
	return tags, f.tagWarnings, nil
}

type fakeMentions []string

func (f fakeMentions) Mentions(string) []string { return f }

// workRegistry registers the five tools over a fake tracker.
func workRegistry(t *testing.T, deps builtin.WorkDeps) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{Work: deps}); err != nil {
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
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	seatCallable, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s is not seat-callable", name)
	}
	got, err := seatCallable.CallForTurn(t.Context(), workTurn(t), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// ---- the cases -------------------------------------------------------- //

// THE THREE WRITES COUNT AS A DELIVERY. A turn woken by an assignment answers
// by moving the item, commenting on it, or filing the follow-up — and without
// this the gate sees only builtins, concludes the turn reached nobody, and
// corrects it into another round.
func TestTheTrackerWritesCountAsDeliveries(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Merges: trk.merges})

	deliveries := reg.Deliveries()
	for _, name := range builtin.WorkWrites() {
		// AND ON THE TRACKER. The name alone says the turn reached
		// somebody; the surface says whom, and without it a tracker write
		// discharged an obligation owed to a founder waiting in chat.
		if deliveries[name] != tracker.Source {
			t.Errorf("%s delivers to %q, want %q — a turn that answered with it "+
				"would otherwise be corrected for having done nothing, or credited "+
				"for answering somebody it never reached",
				name, deliveries[name], tracker.Source)
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

// IDENTITY COMES FROM THE TURN, NEVER FROM ARGUMENTS. A model that could name
// its own actor could file work as anybody.
func TestAWriteIsAttributedToTheTurnsSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "new work", "project": "ENG",
		// A model trying to act as somebody else.
		"actor": "ceo", "reporter": "ceo", "handle": "ceo",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	if len(trk.actors) != 1 {
		t.Fatalf("actors = %v", trk.actors)
	}
	actor := trk.actors[0]
	if actor.Handle != "eng" || actor.Kind != tracker.AuthorAgent {
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
	// THE FAKE LANDS EVERY CREATE AT S@1:11, and the answer says so in
	// the parseable form rather than as three numbers a caller would
	// have to reassemble.
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
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Merges: trk.merges, Search: trk,
		ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
	})
	for _, name := range builtin.WorkTools() {
		entry, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s is not registered", name)
		}
		got, err := entry.Tool.Call(t.Context(), map[string]any{"item": "ENG-1", "title": "x", "body": "y"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Failed || !strings.Contains(got.Output, "during a turn") {
			t.Errorf("%s outside a turn gave %q", name, got.Output)
		}
	}
}

// A COMPANY RUNNING JIRA GETS NO NATIVE TOOLS AT ALL. A seat offered a tool
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
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
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
	if got := callWork(t, reg2, builtin.CommentOnWorkTool, map[string]any{
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
// rather than being another guess.
func TestABadEnumIsRefusedWithTheValidValues(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	// THE CLOSE REASON IS THE STATUS. The tracker has no separate field:
	// `cancelled` IS "finished without being delivered", which is what
	// makes it invisible to a delivery count with no second value to keep in step.
	for _, tc := range []struct{ field, value, want string }{
		{"status", "wip", "in_progress"},
		{"status", "wontfix", "cancelled"},
		{"priority", "P0", "urgent"},
	} {
		got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", tc.field: tc.value,
		})
		if !got.Failed {
			t.Errorf("%s=%q was accepted", tc.field, tc.value)
			continue
		}
		if !strings.Contains(got.Output, tc.want) {
			t.Errorf("the refusal of %s=%q does not list the valid values: %s",
				tc.field, tc.value, got.Output)
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
		got := callWork(t, reg, name, map[string]any{"item": "ENG-1"})
		if !got.Failed {
			t.Errorf("%s reported success on a failed read", name)
		}
		if !strings.Contains(got.Output, "NOT an empty result") {
			t.Errorf("%s does not say the difference: %s", name, got.Output)
		}
	}
}

// A FAILED WRITE SAYS THE CHANGE WAS NOT MADE, or the model reports work it
// did not do.
func TestAFailedWriteSaysSo(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = errors.New("the broker is unreachable")
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || !strings.Contains(got.Output, "NOT made") {
		t.Errorf("a failed write gave %q", got.Output)
	}

	// AND THE HAND-OFF BUDGET TELLS IT WHAT TO DO INSTEAD, rather than
	// inviting another attempt.
	trk.writeErr = tracker.ErrReassignmentBudget
	got = callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "assignee": "ops",
	})
	if !strings.Contains(got.Output, "Do not reassign it again") {
		t.Errorf("the budget refusal invites another attempt: %q", got.Output)
	}

	// AND A WALK THAT STOPPED AT AN UNKNOWN STEP IS NEITHER: some of it may
	// have landed, so "NOT made" would be false, and the same call again is
	// what finishes it.
	trk.writeErr = fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved)
	got = callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || strings.Contains(got.Output, "NOT made") ||
		!strings.Contains(got.Output, "exactly the same arguments") {
		t.Errorf("a walk that stopped at an unresolved step gave %q", got.Output)
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
	// A GESTURE, AND NEVER THE WHOLE SET. [tracker.TaskPatch]'s
	// collections are carried whole, so stating this one edge as the
	// collection deleted every other relation the item had — and the
	// `waiting_on` edges are the expensive half, because their mirrors on
	// the blockers survive and the repair scans for a missing mirror from
	// the AUTHORED end, which is the row that was deleted.
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

// EVERY INERT EDGE ONE CALL STATES TRAVELS IN ONE GESTURE.
//
// A patch carries exactly one relation gesture and the writer resolves it
// against the item's own rows. Two arms composing two gestures means whichever
// ran last wins and the other's edges are silently gone — and two arms
// composing one gesture and one whole SET is worse still: the writer refuses
// the pair outright, so a model that named a link and a duplicate in one call
// was told its own arguments were a programming error.
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
	var kinds []tracker.RelationKind
	for _, add := range gesture.Add {
		kinds = append(kinds, add.Kind)
	}
	for _, want := range []tracker.RelationKind{
		tracker.RelationLinked, tracker.RelationDuplicates,
	} {
		if !slices.Contains(kinds, want) {
			t.Errorf("the gesture adds %v and not %q — the arm that ran "+
				"second replaced the first one's edges rather than joining "+
				"them", kinds, want)
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
	if !slices.Equal(trk.ifMatch, []uint64{7, tracker.NoIfMatch}) {
		t.Errorf("if_match reached the writer as %v, want [7 0]", trk.ifMatch)
	}
}

// A STALE VERSION TELLS THE MODEL TO RE-READ. Told only "it failed", a model
// re-sends the same patch against the same moved item forever.
func TestAStaleVersionSaysToReadItAgain(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = fmt.Errorf("%w: task i1 is at version 9", tracker.ErrStaleVersion)
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
}

// THE ANNOTATIONS ARE WHAT THE WORKER GUARD READS. A write to a surface the
// whole company sees must not be reachable by a sub-agent acting under its
// parent's name.
func TestTheTrackerWritesAreClassifiedAsSharedWrites(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Merges: trk.merges})

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
// name.
//
// The tool's arguments are the MODEL's vocabulary and the query keys are the
// transport's, so the two are translated — and a translation nothing checks is
// one that silently stops translating. Both halves of this tool's table were
// wrong at once: `text` was copied through under its own name, which the
// grammar does not read, so every text search returned an unfiltered list; and
// `open_only` named two "status groups" that do not exist, so the parser
// refused the whole call and the commonest filter a model reaches for failed
// every time.
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
	case len(q.Status) != 1 || q.Status[0] != tracker.StatusTodo:
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
	want := []tracker.StatusGroup{tracker.GroupNotStarted, tracker.GroupActive}
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

// NO SEAT HOLDS AN OPERATOR-ONLY TOOL.
//
// Each of them is a decision a PERSON makes about how the company runs: what
// its vocabulary is, how a tab strip is arranged, what somebody's queue and
// inbox are. A seat given any of them is a seat editing
// the rules it is judged by — and the two that would matter most are the
// catalogue (a create refused for an undeclared type is a signal a person
// needs to see, not one the seat should widen away) and the person record (a
// seat is not a human; it has a mailbox rather than an inbox).
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
		Reader: trk, Writer: trk.as, Merges: trk.merges, Search: trk,
		ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
		CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
		PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
		TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
		ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return trk },
	})
	for _, name := range tracker.OperatorOnlyTools() {
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
			Reader: trk, Writer: trk.as, Merges: trk.merges, Search: trk,
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
			ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return trk },
			Inbox:           trk,
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
	}) {
		operator[tool.Name()] = true
	}
	for _, name := range tracker.OperatorOnlyTools() {
		if !operator[name] {
			t.Errorf("%s is operator-only and the operator surface does not "+
				"serve it, so nothing serves it at all", name)
		}
	}
	// AND THE SEAT'S OWN ARE THERE TOO: the operator surface is the
	// same tools with one field different, not a second catalogue.
	for _, name := range tracker.Tools() {
		if !operator[name] {
			t.Errorf("the operator surface does not serve %s, which every "+
				"seat holds", name)
		}
	}
}

// A SEAT IS NOT TOLD A POPULATED BOARD IS EMPTY.
//
// A grouped answer has no flat rows by construction, and the empty message
// asked only about those — so a board with five columns came back as "No work
// items match that filter", and a seat that believed it would file the
// duplicate.
func TestAGroupedAnswerIsNotReportedAsEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.answer = &tracker.Answer{
		Groups: []tracker.Group{{
			Key: "todo", Count: 12,
			Rows: []tracker.TaskRow{{ID: "t-1", Key: "ENG-1"}},
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

// AND A BOARD OF EMPTY COLUMNS IS EMPTY.
//
// A closed axis carries every column the query admits whether or not anything
// is in it, so "the answer has groups" stopped meaning "the answer has work":
// a seat handed three columns at zero and told nothing would read them as a
// board and go looking for the rows.
func TestABoardWhoseEveryColumnIsEmptyIsReportedEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.answer = &tracker.Answer{
		Groups: []tracker.Group{
			{Key: "todo", Rows: []tracker.TaskRow{}},
			{Key: "in_progress", Rows: []tracker.TaskRow{}},
			{Key: "in_review", Rows: []tracker.TaskRow{}},
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

// EVERY OPERATOR TOOL ANSWERS WITHOUT A TURN.
//
// The operator surface calls through Callable.Call, which passes a nil turn,
// and supplies its own Actor because there is no seat to derive one from. Four
// reads opened with `turn.RequireSeat()` BEFORE consulting that Actor, so the
// whole read half of /operator/mcp refused every call while the writes beside
// them worked — and a catalogue whose reads all fail is one an assistant stops
// trusting entirely.
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
			return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
		},
	}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Work: work}) {
		// THE READS ONLY. A write called with no arguments refuses on
		// its own missing arguments, which is correct and says nothing
		// about the turn; what this case is about is the prelude that
		// refuses BEFORE any argument is read.
		if !strings.HasPrefix(tool.Name(), "list_") &&
			!strings.HasPrefix(tool.Name(), "get_") {
			continue
		}
		result, err := tool.Call(t.Context(), map[string]any{})
		if err != nil {
			t.Fatalf("%s: %v", tool.Name(), err)
		}
		if strings.Contains(result.Output, "can only be called during a turn") {
			t.Errorf("%s refuses outside a turn even though the surface "+
				"supplies its own actor — the identity check must be "+
				"WorkDeps.actor, not turn.RequireSeat", tool.Name())
		}
	}
}

// Depend records the dependency change the tool composed.
//
// THE COMPOSITION IS THE HALF THAT CAN BE WRONG HERE: the tool turns keys into
// ids and a `{set}` gesture into the adds and removes a two-ended write needs,
// and the sequence itself is certified against a real store elsewhere.
// Inbox answers one notice, which is all the placement test needs: what it is
// about is whether the surface SERVES the verb, never what the verb returns.
func (f *fakeTracker) Inbox(_ context.Context, q tracker.InboxQuery,
	_ time.Time) (tracker.InboxAnswer, error) {

	f.inboxQuery = q
	return tracker.InboxAnswer{
		Handle:         q.Who.Handle,
		PrimaryReasons: tracker.DefaultPrimaryReasons,
		Notices: []tracker.InboxNotice{{
			RecordID: "rec-1", SubjectID: "i1", SubjectKey: "ENG-1",
			Reason: tracker.ReasonAssignee, Primary: true,
		}},
		Primary: 1, Unread: 1,
	}, nil
}

func (f *fakeTracker) Depend(_ context.Context, _ string,
	change tracker.DependencyChange, _ tracker.Leads) (tracker.DependencyResult, error) {

	f.depended = append(f.depended, change)
	if f.dependErr != nil {
		return tracker.DependencyResult{}, f.dependErr
	}
	return tracker.DependencyResult{WriteResult: tracker.WriteResult{
		Result: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: 41},
		},
	}}, nil
}

// EVERY FILTER THIS TOOL DECLARES REACHES THE QUERY, and the ones a seat
// could not ask for at all were most of the grammar.
//
// The board's query language compiles forty-odd keys, every one indexed and
// reachable from the dashboard and the REST route, and this tool forwarded
// ten. So a seat could not ask for the bugs, for what is due this week, for
// what moved since yesterday, for the subtasks of one item, for what it filed
// or follows — or for a page after the first, which is the one that makes a
// long answer usable at all. It also could not name a CUSTOM FIELD, which is
// the whole point of a company declaring one.
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

// A PAGING ARGUMENT NEEDS A PAGE TO COME FROM.
//
// `cursor` tells a caller to pass "the `next_cursor` from a previous call",
// and this tool was the one reader of the query grammar that never put one in
// its answer — the REST route beside it has always passed it on. An argument
// whose only source is an answer the tool does not give is an argument nobody
// can use.
func TestTheListAnswerCarriesTheCursorItsOwnArgumentAsksFor(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.answer = &tracker.Answer{
		Complete:   true,
		Rows:       []tracker.TaskRow{{ID: "i1", Key: "ENG-1", Title: "one"}},
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
// `parent_id`, which holds one.
//
// A model types the key it read. Forwarded raw, `parent: ENG-1` compares a key
// against an id column and answers an EMPTY LIST with no error — which reads
// as "that item has no subtasks" rather than as a refusal, so nobody finds out.
// It is the same failure `references` carries its own comment about in the
// parser, two lines from where this one is read.
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

// A PATCH WHOSE ONLY ARGUMENT IS A CUSTOM FIELD OR A RE-ROUTE IS A WRITE.
//
// The emptiness check decides whether the write is issued at all, so a field
// it does not see is a call that answers `outcome: applied` and changes
// nothing — the one failure a seat cannot detect, because there is no error
// to read and no history row to notice is missing. Both of these were in that
// state: `fields` and `routing_unit` were the two arms of the argument parser
// the hand-written list of patch fields had never been extended for.
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
			reg := projectRegistry(t, trk, leadAlways)
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

// OPENING ONE COMMENT IS A READ A MODEL CAN REACH, AND A WRONG ID SAYS SO.
//
// The thread page carries EXCERPTS — twenty bodies at their full length is ten
// times the ceiling on one tool answer — and that is only honest while the
// rest is one call away. It was not: the excerpt was documented as a pointer
// to a `comment:` argument this tool never had, so a body past 2 KiB could not
// be recovered by any seat through any tool. These two cases are the argument
// and the refusal that make the excerpt a pointer rather than a loss.
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
	trk.readErr = fmt.Errorf("%w: cm-nope on i1", tracker.ErrNoComment)
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

// THE DESCRIPTION IS BOUNDED LIKE EVERY OTHER PART OF A DETAIL READ, AND THE
// WHOLE OF IT IS ONE CALL AWAY.
//
// It was the one value on this answer with no bound at all: the thread is
// paged, the history is capped, the relation sets are capped, and the body
// rode whole at up to [tracker.MaxBody]. That cap was exactly
// [builtin.ToolAnswerBytes], so an item with a long description was refused
// for weight — and `include` names the collections BESIDE the task, never the
// task itself, so the refusal's own advice could not help. A caller who does
// mean the description says so and gets it whole.
func TestADescriptionIsShortenedUnlessItIsWhatWasAskedFor(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	whole := strings.Repeat("d", builtin.TaskBodyShown*3)
	item := trk.tasks["ENG-1"]
	item.Task.Body = whole
	trk.tasks["ENG-1"] = item
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk})

	// BY DEFAULT IT IS CUT, AND MARKED — an unmarked cut reads as a
	// description that ended there, and the reader never learns there is
	// more to ask for.
	got := callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"})
	if got.Failed {
		t.Fatalf("an item with a long description failed: %q", got.Output)
	}
	if strings.Contains(got.Output, whole) {
		t.Error("the whole description rode on an ordinary read, which is " +
			"what put a maximal item past the ceiling with no argument to narrow it")
	}
	if !strings.Contains(got.Output, "…") {
		t.Errorf("the description was cut without a mark: %.300s", got.Output)
	}

	// AND `body: true` RETURNS IT WHOLE — the half that makes the cut a
	// pointer rather than a loss.
	got = callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "body": true,
	})
	if got.Failed {
		t.Fatalf("asking for the description failed: %q", got.Output)
	}
	if !strings.Contains(got.Output, whole) {
		t.Error("`body: true` did not return the description whole, so what " +
			"was written past the cut is reachable through no tool at all")
	}
	// ON ITS OWN, like `comment:` — a whole description carried beside the
	// thread, the history and the links is the shape the ceiling refuses.
	if trk.wants.Comments || trk.wants.History || trk.wants.Links || trk.wants.Fields {
		t.Errorf("asking for the description also asked for comments=%v "+
			"history=%v links=%v fields=%v", trk.wants.Comments,
			trk.wants.History, trk.wants.Links, trk.wants.Fields)
	}

	// AND THE TWO WHOLE-VALUE READS DO NOT COMPOSE. Together they are 64
	// KiB before escaping against a 64 KiB ceiling, so a call naming both
	// would be refused for asking for exactly the two things these
	// arguments exist to make reachable; the narrower ask wins.
	got = callWork(t, reg, builtin.GetWorkItemTool, map[string]any{
		"item": "ENG-1", "body": true, "comment": "cm-9",
	})
	if got.Failed {
		t.Fatalf("naming both failed: %q", got.Output)
	}
	if trk.wants.Comment != "cm-9" || strings.Contains(got.Output, whole) {
		t.Errorf("naming both gave comment=%q and a whole body=%v — one of "+
			"them has to win, and it is the narrower",
			trk.wants.Comment, strings.Contains(got.Output, whole))
	}
}

// WHEN A TASK IS DUE AND HOW BIG IT IS.
//
// All four columns have existed since migration 0002, the query grammar
// filters on every one and sorts on three, a row's `overdue` flag is derived
// from the due date, and every total is a sum over the sizing pair. NOTHING
// COULD SET ANY OF THEM — so an estimate could never exist, and both the
// overdue predicate and every size total were dead surface that looked like
// an empty company.
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
	// A CLEAR IS A VALUE, not an absent field: nil on a patch means "not
	// named". The applier reads the zero instant as empty.
	if cleared.DueAt == nil || !cleared.DueAt.IsZero() {
		t.Errorf("a cleared due date is %v, want the zero instant the applier "+
			"reads as empty", cleared.DueAt)
	}
}

// A SIZE IS NOT NEGATIVE, and one is refused naming the rule rather than
// stored — a negative estimate would be subtracted from its own board's
// total.
func TestTheSchedulingValuesAreRefusedRatherThanStoredWrong(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	for _, args := range []map[string]any{
		{"title": "x", "project": "ENG", "points": -3},
		{"title": "x", "project": "ENG", "estimate_minutes": -60},
	} {
		if got := callWork(t, reg, builtin.CreateWorkItemTool, args); !got.Failed {
			t.Errorf("create(%v) was accepted, want a refusal naming the rule", args)
		}
	}
}

// AN UNREADABLE SIZE IS REFUSED, NOT READ AS ZERO.
//
// This is the date rule above applied to the half of `readSchedule` that was
// not written to it. Each of these fields is a POINTER because its zero is a
// setting — zero minutes and zero points both mean UNESTIMATED — so a
// value the parser could not read became `&0`, the write succeeded, and the
// answer said `applied` while the estimate had been WIPED. A model reads that
// as having set one.
//
// `"2 days"` is the case that makes it more than a missed refusal: the reader
// was `fmt.Sscanf("%d")`, which takes the leading integer and stops, so a
// two-day estimate was stored as two MINUTES with nothing to say so.
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

// A SIZE THAT IS NOT A NUMBER IS REFUSED, and NaN is the one that gets past a
// range check by definition: every comparison with it is false, so the
// `points < 0` guard said nothing about it and it reached the writer. An
// infinity passed the same guard honestly. Neither is a value a total's
// figures can be summed from, and JSON cannot encode either — so the failure
// would have surfaced somewhere downstream with no memory of who typed it.
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
		// `int(+Inf)` is not defined by the language: it lands on the
		// platform's minimum int, which the negative check then refused
		// as a NEGATIVE estimate — the right answer for the wrong reason,
		// naming a sign nobody typed.
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

// THE UPDATE SCHEMA ADMITS THE NULL ITS OWN DESCRIPTION PROMISES.
//
// Clearing a date or a size is done by passing null; `readSchedule`
// reads it and the description says so. The declared type said `string` and
// `integer` alone, so a caller that VALIDATES against this schema refuses the
// null before the tool is reached — a gesture documented, implemented, and
// unreachable through any strict client.
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

// A SCHEDULE EDIT REACHES THE WAKE IT ANNOUNCES.
//
// The notification's deltas are computed between the task this tool READ and
// a snapshot of it with the patch applied. That snapshot was a field-by-field
// reimplementation of the writer's own merge, and it never learned the
// schedule fields — so the durable row took the new due date while the
// snapshot kept the old one, `TaskDeltas` compared a task against itself on
// exactly those fields, and every date, estimate and size a seat moved
// arrived as a change that changed nothing.
//
// The snapshot is the writer's merge now, so this asserts the CONSEQUENCE
// rather than the copy: a test over the field list would pass again the day
// somebody adds a field to the patch and forgets it, which is the bug.
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
