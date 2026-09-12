package tracker_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN IF-MATCH IS COMPARED INSIDE THE SNAPSHOT THAT DECIDES.
//
// This is the write authority's own sentence applied to a caller's
// precondition: the version the check reads and the record the decision
// publishes come from ONE transaction, so there is no window in which the
// version moves between the comparison and the append. A check performed
// before the snapshot — the obvious shape, and the one a caller writes when
// the framework does not offer this — reads a version, then publishes against
// a state that has already moved, and reports a conditional write that was in
// fact unconditional.
func TestAnIfMatchIsRefusedFromTheSnapshotThatDecides(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the contended one")

	// THE VERSION IS THE PACKED POSITION, which is what a caller reads
	// back from get_work_item and hands to the next edit.
	if _, err := r.writer.UpdateTask(t.Context(), "op-match", task.ID, "ENG",
		task.Version, tracker.TaskPatch{Title: ptr("edited")}, tracker.ChangeFields, nil); err != nil {
		t.Fatalf("an edit at the version it read was refused: %v", err)
	}
	r.drain()

	// AND THE SAME EXPECTATION AGAIN IS NOW STALE, because the write above
	// moved the task — which is exactly the second writer's case.
	_, err := r.writer.UpdateTask(t.Context(), "op-stale", task.ID, "ENG",
		task.Version, tracker.TaskPatch{Title: ptr("clobbered")}, tracker.ChangeFields, nil)
	if !errors.Is(err, tracker.ErrStaleVersion) {
		t.Fatalf("an edit conditioned on a version that has moved gave %v, "+
			"want ErrStaleVersion — a caller told anything else re-sends the "+
			"same patch against the same moved task", err)
	}
	r.drain()
	answer := r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 1 || answer.Rows[0].Title != "edited" {
		t.Errorf("the refused edit reached the rows anyway: %+v", answer.Rows)
	}

	// ZERO MERGES, which is the default every caller naming two fields
	// wants: it is not a precondition of "version 0".
	if _, err := r.writer.UpdateTask(t.Context(), "op-merge", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("merged")}, tracker.ChangeFields, nil); err != nil {
		t.Fatalf("an unconditional edit was refused: %v", err)
	}
	r.drain()
	if answer = r.ask(map[string]any{"container": "project:ENG"}); answer.Rows[0].Title != "merged" {
		t.Errorf("NoIfMatch did not merge: %+v", answer.Rows)
	}
}

// THE HAND-OFF BUDGET IS SPENT BY AGENTS AND FORGIVEN BY PEOPLE.
//
// The failure it exists to stop is a LOOP — two seats each convinced the other
// owns an item — which is a property of the item and not of any one turn's
// delegation depth. So the counter is on the task, it is charged only when an
// agent moves the assignee, and any human touch returns the whole budget.
func TestTheHandOffBudgetIsChargedToTheItem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the circulating one")

	agent := r.writer.As("eng", tracker.AuthorAgent, tracker.Provenance{TurnID: "turn-1"})
	for i := range tracker.ReassignmentBudget {
		to := fmt.Sprintf("peer-%d", i)
		if _, err := agent.UpdateTask(t.Context(), fmt.Sprintf("op-hand-%d", i),
			task.ID, "ENG", tracker.NoIfMatch,
			tracker.TaskPatch{Assignee: &to}, tracker.ChangeAssignee, nil); err != nil {
			t.Fatalf("hand-off %d of %d was refused: %v",
				i+1, tracker.ReassignmentBudget, err)
		}
		r.drain()
	}

	over := "peer-last"
	_, err := agent.UpdateTask(t.Context(), "op-hand-over", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Assignee: &over}, tracker.ChangeAssignee, nil)
	if !errors.Is(err, tracker.ErrReassignmentBudget) {
		t.Fatalf("hand-off %d gave %v, want ErrReassignmentBudget",
			tracker.ReassignmentBudget+1, err)
	}

	// A NO-OP ASSIGNMENT IS NOT A HAND-OFF, so a redelivered record cannot
	// exhaust a budget nobody spent. Charged at the budget's own edge, so
	// the case fails if the exemption is dropped.
	same := fmt.Sprintf("peer-%d", tracker.ReassignmentBudget-1)
	if _, err := agent.UpdateTask(t.Context(), "op-hand-same", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Assignee: &same}, tracker.ChangeAssignee, nil); err != nil {
		t.Fatalf("re-asserting the current assignee was charged: %v", err)
	}
	r.drain()

	// A PERSON TOUCHING IT AT ALL RETURNS THE BUDGET — whatever they
	// changed. Somebody looked, which is the condition the counter exists
	// to detect the absence of.
	if _, err := r.writer.UpdateTask(t.Context(), "op-human", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("a person looked")}, tracker.ChangeFields,
		nil); err != nil {
		t.Fatalf("the human touch was refused: %v", err)
	}
	r.drain()

	after := "peer-again"
	if _, err := agent.UpdateTask(t.Context(), "op-hand-after", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Assignee: &after}, tracker.ChangeAssignee, nil); err != nil {
		t.Fatalf("the budget was not returned by the human touch: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), task.ID, tracker.DetailWants{},
		statelog.ReadSession)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if detail.Task.Reassignments != 1 {
		t.Errorf("reassignments = %d after a reset and one hand-off, want 1 — "+
			"the counter is a value the writer decides and every node applies, "+
			"never one each node derives for itself", detail.Task.Reassignments)
	}
	if detail.Task.Assignee != after {
		t.Errorf("assignee = %q, want %q", detail.Task.Assignee, after)
	}
}

// A WRITER CANNOT ACT AS NOBODY, and the refusal reaches the CALL rather than
// the derivation.
//
// [tracker.Writer.As] is the second door into the same state [tracker.NewWriter]
// guards, and a surface deriving an identity has nothing useful to do with an
// error at that point — so the refusal is carried and surfaces at the write it
// would have made. Every path publishes through one funnel, which is what
// makes that one guard rather than one per method.
func TestAWriterWithNoIdentityRefusesAtTheWrite(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the attributed one")

	for name, w := range map[string]*tracker.Writer{
		"no author":      r.writer.As("", tracker.AuthorOperator, tracker.Provenance{}),
		"no author kind": r.writer.As("ops", "", tracker.Provenance{}),
		"a bogus kind":   r.writer.As("ops", tracker.AuthorKind("root"), tracker.Provenance{}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.UpdateTask(t.Context(), "op-anon", task.ID, "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("anonymous")}, tracker.ChangeFields, nil)
			if err == nil {
				t.Fatal("an unattributable writer wrote a history row")
			}
			if !strings.Contains(err.Error(), "carries who wrote it") {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}
	r.drain()
	if answer := r.ask(map[string]any{"container": "project:ENG"}); answer.Rows[0].Title != "the attributed one" {
		t.Errorf("an unattributable write reached the rows: %+v", answer.Rows)
	}
}
