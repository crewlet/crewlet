package worklet_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// waitsOn makes dependent wait on blocker.
func waitsOn(t *testing.T, r *roundTrip, dependent, blocker string) {
	t.Helper()
	if _, err := r.writer.Depend(t.Context(), "op-"+dependent+"-on-"+blocker, worklet.DependencyChange{
		Task: dependent, Project: "ENG", WaitingOnAdd: []string{blocker},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend %s on %s: %v", dependent, blocker, err)
	}
	r.drain()
}

// WHAT A PERSON HOLDS THAT OTHER WORK WAITS ON, the most waited on first.
//
// The owner holds two items. One is waited on by two items, one of those by
// a third, and two of them by a fourth (counted once); a finished item that
// waited on it counts nothing. The other is waited on by one item. Nothing
// the walk reads is the block's own predicate: the counts are the fixture's.
func TestAPersonsHeldItemsRankByWhatTheyBlock(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for id, holder := range map[string]string{
		"a": "owner", "f": "owner",
		"b": "pm", "e": "pm", "k": "pm", "c": "backend", "z": "qa", "g": "backend",
		"own": "owner",
	} {
		assign(t, r, id, holder)
	}
	waitsOn(t, r, "b", "a")
	waitsOn(t, r, "e", "a")
	waitsOn(t, r, "k", "a")
	waitsOn(t, r, "c", "b")
	waitsOn(t, r, "z", "b")
	waitsOn(t, r, "z", "e")
	waitsOn(t, r, "g", "f")
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-k-done", "k", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("finish k: %v", err)
	}
	r.drain()

	got := r.myWork("owner")
	var order []string
	for _, row := range got.HoldingUp {
		order = append(order, row.ID)
	}
	if !slices.Equal(order, []string{"a", "f"}) {
		t.Fatalf("holding_up = %v, want a (four wait on it) before f (one), and "+
			"nothing for the item nothing waits on", order)
	}
	a, f := got.HoldingUp[0], got.HoldingUp[1]
	if a.Blocks != 4 || !slices.Equal(a.Holders, []string{"backend", "pm", "qa"}) {
		t.Errorf("a blocks %d held by %v, want 4 (b, e, c, z once; the finished k "+
			"counts nothing) held by backend, pm and qa", a.Blocks, a.Holders)
	}
	if f.Blocks != 1 || !slices.Equal(f.Holders, []string{"backend"}) {
		t.Errorf("f blocks %d held by %v, want 1 held by backend", f.Blocks, f.Holders)
	}
	if a.HeldSince.IsZero() {
		t.Error("a carries no held-since instant")
	}
	if got.Totals.HoldingUp.Total != 2 {
		t.Errorf("the block's total = %+v, want the two held items", got.Totals.HoldingUp)
	}
	// AND pm, who holds b and e, which c and z wait on: one block per person.
	pm := r.myWork("pm")
	if len(pm.HoldingUp) != 2 || pm.HoldingUp[0].ID != "b" || pm.HoldingUp[0].Blocks != 2 ||
		pm.HoldingUp[1].ID != "e" || pm.HoldingUp[1].Blocks != 1 {
		t.Errorf("pm's holding_up = %+v, want b (c and z) before e (z)", pm.HoldingUp)
	}
}

// A PERSON'S OWN WORK WAITING ON THEIR OWN ITEM COUNTS, AND NAMES NOBODY ELSE.
// What waits is work either way; who it holds up is the holders list, and the
// person is never one of them.
func TestAHeldItemNeverNamesItsHolderAsWaiting(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "mine", "owner")
	assign(t, r, "next", "owner")
	assign(t, r, "theirs", "pm")
	waitsOn(t, r, "next", "mine")
	waitsOn(t, r, "theirs", "next")

	got := r.myWork("owner")
	if len(got.HoldingUp) != 2 {
		t.Fatalf("holding_up = %+v, want both of the owner's items", got.HoldingUp)
	}
	first := got.HoldingUp[0]
	if first.ID != "mine" || first.Blocks != 2 || !slices.Equal(first.Holders, []string{"pm"}) {
		t.Errorf("first row = %s blocking %d held by %v, want mine blocking 2, held by pm alone",
			first.ID, first.Blocks, first.Holders)
	}
}

// AN ITEM HANDED OVER IN A STATUS CHANGE IS HELD FROM THAT CHANGE, in Holding
// up and in the advisor's idle reviews alike: both read one hand-over instant,
// whichever change moved the assignee, and it is when the item entered review.
func TestAHandOverInAStatusChangeIsOneInstantForEveryReader(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "a", "app")
	assign(t, r, "b", "pm")
	waitsOn(t, r, "b", "a")
	review, owner := worklet.StatusInReview, "owner"
	if _, err := r.writer.UpdateTask(t.Context(), "op-a-review", "a", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &review, Assignee: &owner}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("hand a over in review: %v", err)
	}
	r.drain()
	entered := r.task(t, "a").Task.StatusEnteredAt
	held := r.myWork("owner").HoldingUp
	if len(held) != 1 || !held[0].HeldSince.Equal(entered) {
		t.Fatalf("holding up is %+v, want a held since it entered review at %s", held, entered)
	}
	work, err := r.reader.WorkPatterns(t.Context(), worklet.PatternsQuery{
		Since: entered.Add(-time.Hour), Until: entered.Add(time.Hour), Level: statelog.ReadStale,
		Reviewers: []string{"owner"}, QuietSince: entered.Add(time.Hour)})
	if err != nil {
		t.Fatalf("WorkPatterns: %v", err)
	}
	if len(work.IdleReviews) != 1 || !work.IdleReviews[0].Since.Equal(held[0].HeldSince) {
		t.Errorf("the idle review is %+v, want it waiting since %s, as Holding up reads it",
			work.IdleReviews, held[0].HeldSince)
	}
}

// myWorkWith is [roundTrip.myWork] told the company's agent seats.
func (r *roundTrip) myWorkWith(handle string, agents []string) worklet.MyWork {
	r.t.Helper()
	out, err := r.reader.MyWork(r.t.Context(), worklet.MyWorkQuery{
		Handle: handle, Level: statelog.ReadStale, Agents: agents,
	}, wednesday, time.UTC)
	if err != nil {
		r.t.Fatalf("MyWork(%s): %v", handle, err)
	}
	return out
}

// WHAT A PERSON HOLDS UP COUNTS THE AGENT WORK BEHIND IT (ADR-0124): an item
// assigned to them, and an item an agent asked them about, which itself waits
// on their answer; through each, the agent items waiting in the chains behind
// it; and across both, each agent item once.
//
// Mutation: leave out the items held by an ask and q's chain is missing; count
// per row in the summary and z is counted twice.
func TestHoldingUpCountsTheAgentWorkWaitingOnAPerson(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for id, holder := range map[string]string{
		"a": "owner", "b": "pm", "c": "lena", "q": "backend", "rr": "qa", "s": "lena", "z": "pm",
	} {
		assign(t, r, id, holder)
	}
	waitsOn(t, r, "b", "a")
	waitsOn(t, r, "c", "a")
	waitsOn(t, r, "z", "a")
	waitsOn(t, r, "rr", "q")
	waitsOn(t, r, "s", "q")
	waitsOn(t, r, "z", "q")
	askOn(t, r, "op-ask", "q", worklet.Comment{ID: "c-ask", Task: "q", Author: "backend",
		AuthorKind: worklet.AuthorAgent, Body: "Merge it?", Ask: "owner", CreatedAt: wednesday})

	agents := []string{"pm", "backend", "qa"}
	got := r.myWorkWith("owner", agents)
	if len(got.HoldingUp) != 2 {
		t.Fatalf("holding_up = %+v, want a, held by assignment, and q, held by a question", got.HoldingUp)
	}
	q, a := got.HoldingUp[0], got.HoldingUp[1]
	if q.ID != "q" || q.Asked != "c-ask" || q.Blocks != 3 || q.AgentWaiting != 3 ||
		!slices.Equal(q.Holders, []string{"backend", "lena", "pm", "qa"}) {
		t.Errorf("first row = %s asked %q blocking %d with %d agent items held by %v; want q, "+
			"held by c-ask, blocking 3, with 3 agent items (q itself, rr, z), held by backend, "+
			"lena, pm and qa", q.ID, q.Asked, q.Blocks, q.AgentWaiting, q.Holders)
	}
	if a.ID != "a" || a.Asked != "" || a.Blocks != 3 || a.AgentWaiting != 2 {
		t.Errorf("second row = %s asked %q blocking %d with %d agent items; want a, assigned, "+
			"blocking 3 with 2 agent items (b, z)", a.ID, a.Asked, a.Blocks, a.AgentWaiting)
	}
	if want := (worklet.HoldingAgents{Items: 2, AgentItems: 4}); got.HoldingAgents != want {
		t.Errorf("the summary = %+v, want %+v: b, z, q and rr, z once", got.HoldingAgents, want)
	}
	if got.Totals.HoldingUp.Total != 2 {
		t.Errorf("the block's total = %+v, want both held items", got.Totals.HoldingUp)
	}
	if none := r.myWorkWith("owner", nil); none.HoldingAgents != (worklet.HoldingAgents{}) {
		t.Errorf("with no agent seats named the summary = %+v, want nothing counted", none.HoldingAgents)
	}
}
