package builtin_test

import (
	"context"
	"errors"
	"fmt"
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
	// query is the last one the list tool built, so a case can assert
	// what a model's ARGUMENTS became — which is the half of this tool
	// nothing looked at while it dropped a filter and refused a flag.
	query tracker.Query
	tasks map[string]tracker.TaskDetail

	created  []tracker.Task
	patched  []tracker.TaskPatch
	ifMatch  []uint64
	notified []*tracker.Notify
	actors   []builtin.Actor
	opIDs    []string

	readErr  error
	writeErr error
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{tasks: map[string]tracker.TaskDetail{
		"ENG-1": {
			Task: tracker.Task{
				ID: "i1", Key: "ENG-1", Project: "ENG", Title: "the work",
				Status: tracker.StatusTodo, StatusGroup: tracker.GroupNotStarted,
				Version: 7,
			},
			Complete: true,
		},
	}}
}

func (f *fakeTracker) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	f.query = q
	if f.readErr != nil {
		return tracker.Answer{}, f.readErr
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

func (f *fakeTracker) Task(_ context.Context, idOrKey string, _ tracker.DetailWants,
	_ statelog.ReadLevel) (tracker.TaskDetail, error) {

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

func (f *fakeTracker) Views(context.Context, tracker.ViewQuery) (tracker.ViewListing, error) {
	return tracker.ViewListing{}, nil
}

func (f *fakeTracker) Goals(context.Context, tracker.GoalQuery) (tracker.GoalListing, error) {
	return tracker.GoalListing{}, nil
}

func (f *fakeTracker) Catalogue(context.Context, tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return tracker.CatalogueAnswer{}, nil
}

func (f *fakeTracker) Person(context.Context, tracker.PersonQuery, time.Time) (tracker.PersonState, error) {
	return tracker.PersonState{}, nil
}

// as records the actor and hands back a writer bound to it, which is the
// tracker's own rule: a writer acts as exactly one party.
func (f *fakeTracker) as(actor builtin.Actor) builtin.WorkWriter {
	f.actors = append(f.actors, actor)
	return f
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
	patch tracker.TaskPatch, notify *tracker.Notify) (tracker.WriteResult, error) {

	if f.writeErr != nil {
		return tracker.WriteResult{}, f.writeErr
	}
	f.patched = append(f.patched, patch)
	f.ifMatch = append(f.ifMatch, ifMatch)
	f.notified = append(f.notified, notify)
	f.opIDs = append(f.opIDs, opID)
	return tracker.WriteResult{
		Key: "ENG-1", Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 12},
		Version:  12,
	}, nil
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
	return &turnctx.Turn{ID: "turn-1", Seat: o.Roles[0], Org: o, Chain: []string{"pm"}}
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
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	deliverables := reg.Deliverables()
	for _, name := range builtin.WorkWrites() {
		if !slices.Contains(deliverables, name) {
			t.Errorf("%s does not count as a delivery, so a turn that answered "+
				"with it would be corrected for having done nothing", name)
		}
	}
	// READING IS NOT DELIVERING. A turn that only read is exactly the turn
	// the gate exists to catch.
	for _, name := range []string{builtin.ListWorkItemsTool, builtin.GetWorkItemTool} {
		if slices.Contains(deliverables, name) {
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
	if actor.TurnID != "turn-1" || !slices.Equal(actor.Chain, []string{"pm"}) {
		t.Errorf("provenance = %+v, want the turn's id and chain", actor)
	}
}

// OUTSIDE A TURN THERE IS NO SEAT, so every one of these refuses rather than
// writing as nobody.
func TestTheTrackerToolsRefuseOutsideATurn(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
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
	// makes it invisible to velocity with no second value to keep in step.
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
	relations := trk.patched[0].Relations
	if relations == nil || len(*relations) != 1 {
		t.Fatalf("relations = %v", relations)
	}
	if got := (*relations)[0].Other; got != "i1" {
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
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

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
// its vocabulary is, what a goal is, how a tab strip is arranged, what
// somebody's queue and inbox are. A seat given any of them is a seat editing
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
		Reader: trk, Writer: trk.as,
		ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
		GoalWriter:      func(builtin.Actor) builtin.GoalWriter { return nil },
		CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
		PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
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
			Reader: trk, Writer: trk.as,
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			GoalWriter:      func(builtin.Actor) builtin.GoalWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
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
	// AND THE SEAT'S OWN SIX ARE THERE TOO: the operator surface is the
	// same tools with one field different, not a second catalogue.
	for _, name := range tracker.Tools() {
		if !operator[name] {
			t.Errorf("the operator surface does not serve %s, which every "+
				"seat holds", name)
		}
	}
}
