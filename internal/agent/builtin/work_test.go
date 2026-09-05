package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/work"
)

// ---- the fakes -------------------------------------------------------- //

type fakeTracker struct {
	items map[string]work.Detail

	created  []work.NewItem
	updated  []work.Edit
	comments []work.NewComment
	actors   []work.Actor
	ifMatch  []uint64

	readErr  error
	writeErr error
}

func newFakeTracker() *fakeTracker {
	return &fakeTracker{items: map[string]work.Detail{
		"ENG-1": {
			Item:     work.Item{ID: "i1", Key: "ENG-1", Project: "ENG", Title: "the work", Status: work.StatusTodo},
			Revision: 7,
		},
	}}
}

func (f *fakeTracker) List(context.Context, work.Filter) ([]work.Summary, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]work.Summary, 0, len(f.items))
	for _, d := range f.items {
		out = append(out, work.Summary{ID: d.Item.ID, Key: d.Item.Key, Title: d.Item.Title})
	}
	return out, nil
}

func (f *fakeTracker) Get(_ context.Context, idOrKey string) (work.Detail, error) {
	if f.readErr != nil {
		return work.Detail{}, f.readErr
	}
	for key, d := range f.items {
		if key == strings.ToUpper(idOrKey) || d.Item.ID == idOrKey {
			return d, nil
		}
	}
	return work.Detail{}, work.ErrNotFound
}

func (f *fakeTracker) Create(_ context.Context, actor work.Actor, in work.NewItem) (work.Written, error) {
	if f.writeErr != nil {
		return work.Written{}, f.writeErr
	}
	f.created = append(f.created, in)
	f.actors = append(f.actors, actor)
	return work.Written{
		Item:     work.Item{ID: "new", Key: "ENG-9", Status: work.StatusTriage, Assignee: in.Assignee},
		Revision: 11,
	}, nil
}

func (f *fakeTracker) Update(_ context.Context, actor work.Actor, _ string, ifMatch uint64, edit work.Edit) (work.Written, error) {
	if f.writeErr != nil {
		return work.Written{}, f.writeErr
	}
	f.updated = append(f.updated, edit)
	f.actors = append(f.actors, actor)
	f.ifMatch = append(f.ifMatch, ifMatch)
	return work.Written{Item: work.Item{Key: "ENG-1", Status: work.StatusInProgress}, Revision: 12}, nil
}

func (f *fakeTracker) Comment(_ context.Context, actor work.Actor, _ string, in work.NewComment) (work.Comment, work.Written, error) {
	if f.writeErr != nil {
		return work.Comment{}, work.Written{}, f.writeErr
	}
	f.comments = append(f.comments, in)
	f.actors = append(f.actors, actor)
	return work.Comment{ID: "m1", Mentions: in.Mentions},
		work.Written{Item: work.Item{Key: "ENG-1"}, Revision: 13}, nil
}

func (f *fakeTracker) Item(context.Context, string) (work.Item, uint64, error) {
	return work.Item{}, 0, work.ErrNotFound
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
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})

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
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})

	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "new work", "project": "ENG",
		// A model trying to act as somebody else.
		"actor": "ceo", "reporter": "ceo", "handle": "ceo",
	})
	if got.Failed {
		t.Fatalf("create failed: %s", got.Output)
	}
	if len(tracker.actors) != 1 {
		t.Fatalf("actors = %v", tracker.actors)
	}
	actor := tracker.actors[0]
	if actor.Handle != "eng" || actor.Kind != work.AuthorAgent {
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
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})
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
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: tracker, Writer: tracker, Mentions: fakeMentions{"pm"},
	})
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "@pm this needs a decision",
	})
	if got.Failed {
		t.Fatalf("comment failed: %s", got.Output)
	}
	if len(tracker.comments) != 1 {
		t.Fatalf("comments = %v", tracker.comments)
	}
	if tracker.comments[0].TurnKey != "turn-1" {
		t.Errorf("the comment carries turn key %q — a re-run turn would post twice",
			tracker.comments[0].TurnKey)
	}
	if !slices.Equal(tracker.comments[0].Mentions, []string{"pm"}) {
		t.Errorf("mentions = %v, want the resolved handles", tracker.comments[0].Mentions)
	}
}

// IF-MATCH IS PASSED THROUGH, so a model that read the item can make its edit
// conditional — and omitting it merges, which is what a model naming two
// fields needs.
func TestIfMatchReachesTheStore(t *testing.T) {
	t.Parallel()
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})

	callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "in_progress", "if_match": 7,
	})
	callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "in_progress",
	})
	if !slices.Equal(tracker.ifMatch, []uint64{7, 0}) {
		t.Errorf("if_match reached the store as %v, want [7 0]", tracker.ifMatch)
	}
}

// A BAD ENUM IS REFUSED WITH THE LIST, so the model's next call is right
// rather than being another guess.
func TestABadEnumIsRefusedWithTheValidValues(t *testing.T) {
	t.Parallel()
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})
	for _, tc := range []struct{ field, value, want string }{
		{"status", "wip", "in_progress"},
		{"priority", "P0", "urgent"},
		{"close_reason", "wontfix", "not_planned"},
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
	tracker := newFakeTracker()
	tracker.readErr = errors.New("the projection is not hydrated yet")
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})

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
	tracker := newFakeTracker()
	tracker.writeErr = errors.New("the broker is unreachable")
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})

	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "status": "done",
	})
	if !got.Failed || !strings.Contains(got.Output, "NOT made") {
		t.Errorf("a failed write gave %q", got.Output)
	}

	// AND THE HAND-OFF BUDGET TELLS IT WHAT TO DO INSTEAD, rather than
	// inviting another attempt.
	tracker.writeErr = work.ErrReassignmentBudget
	got = callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "assignee": "ops",
	})
	if !strings.Contains(got.Output, "Do not reassign it again") {
		t.Errorf("the budget refusal invites another attempt: %q", got.Output)
	}
}

// THE ANNOTATIONS ARE WHAT THE WORKER GUARD READS. A write to a surface the
// whole company sees must not be reachable by a sub-agent acting under its
// parent's name.
func TestTheTrackerWritesAreClassifiedAsSharedWrites(t *testing.T) {
	t.Parallel()
	tracker := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: tracker, Writer: tracker})

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
