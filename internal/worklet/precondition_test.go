package worklet_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN IF-MATCH IS COMPARED INSIDE THE SNAPSHOT THAT DECIDES.
//
// The version the check reads and the record the decision publishes come from
// ONE transaction. A check before the snapshot publishes against a state that
// may have moved, and reports a conditional write that was unconditional.
func TestAnIfMatchIsRefusedFromTheSnapshotThatDecides(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the contended one")

	// THE VERSION IS THE PACKED POSITION, as get_work_item answers it.
	if _, err := r.writer.UpdateTask(t.Context(), "op-match", task.ID, "ENG",
		task.Version, worklet.TaskPatch{Title: ptr("edited")}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("an edit at the version it read was refused: %v", err)
	}
	r.drain()

	// AND THE SAME EXPECTATION AGAIN IS NOW STALE: the second writer's case.
	_, err := r.writer.UpdateTask(t.Context(), "op-stale", task.ID, "ENG",
		task.Version, worklet.TaskPatch{Title: ptr("clobbered")}, worklet.ChangeFields, nil)
	if !errors.Is(err, worklet.ErrStaleVersion) {
		t.Fatalf("an edit conditioned on a version that has moved gave %v, "+
			"want ErrStaleVersion — a caller told anything else re-sends the "+
			"same patch against the same moved task", err)
	}
	r.drain()
	answer := r.ask(map[string]any{"container": "project:ENG"})
	if len(answer.Rows) != 1 || answer.Rows[0].Title != "edited" {
		t.Errorf("the refused edit reached the rows anyway: %+v", answer.Rows)
	}

	// ZERO MERGES: it means no precondition, not "version 0".
	if _, err := r.writer.UpdateTask(t.Context(), "op-merge", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: ptr("merged")}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("an unconditional edit was refused: %v", err)
	}
	r.drain()
	if answer = r.ask(map[string]any{"container": "project:ENG"}); answer.Rows[0].Title != "merged" {
		t.Errorf("NoIfMatch did not merge: %+v", answer.Rows)
	}
}

// THE HAND-OFF BUDGET IS SPENT BY AGENTS AND FORGIVEN BY PEOPLE.
//
// It stops a LOOP of two seats each convinced the other owns an item, a
// property of the item rather than a turn's delegation depth: so the counter
// is on the task, charged when an agent moves the assignee, and any human touch
// returns it.
func TestTheHandOffBudgetIsChargedToTheItem(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the circulating one")

	agent := r.writer.As("eng", worklet.AuthorAgent, worklet.Provenance{WrittenFor: types.ForTurn(nil), TurnID: "turn-1"})
	for i := range worklet.ReassignmentBudget {
		to := fmt.Sprintf("peer-%d", i)
		if _, err := agent.UpdateTask(t.Context(), fmt.Sprintf("op-hand-%d", i),
			task.ID, "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Assignee: &to}, worklet.ChangeAssignee, nil); err != nil {
			t.Fatalf("hand-off %d of %d was refused: %v",
				i+1, worklet.ReassignmentBudget, err)
		}
		r.drain()
	}

	over := "peer-last"
	_, err := agent.UpdateTask(t.Context(), "op-hand-over", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Assignee: &over}, worklet.ChangeAssignee, nil)
	if !errors.Is(err, worklet.ErrReassignmentBudget) {
		t.Fatalf("hand-off %d gave %v, want ErrReassignmentBudget",
			worklet.ReassignmentBudget+1, err)
	}

	// A NO-OP ASSIGNMENT IS NOT A HAND-OFF, so a redelivered record cannot
	// exhaust the budget; charged at the budget's edge, so the case fails
	// if the exemption is dropped.
	same := fmt.Sprintf("peer-%d", worklet.ReassignmentBudget-1)
	if _, err := agent.UpdateTask(t.Context(), "op-hand-same", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Assignee: &same}, worklet.ChangeAssignee, nil); err != nil {
		t.Fatalf("re-asserting the current assignee was charged: %v", err)
	}
	r.drain()

	// A PERSON TOUCHING IT AT ALL RETURNS THE BUDGET, whatever they
	// changed: somebody looked.
	if _, err := r.writer.UpdateTask(t.Context(), "op-human", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: ptr("a person looked")}, worklet.ChangeFields,
		nil); err != nil {
		t.Fatalf("the human touch was refused: %v", err)
	}
	r.drain()

	after := "peer-again"
	if _, err := agent.UpdateTask(t.Context(), "op-hand-after", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Assignee: &after}, worklet.ChangeAssignee, nil); err != nil {
		t.Fatalf("the budget was not returned by the human touch: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), task.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if detail.Task.Reassignments != 1 {
		t.Errorf("reassignments = %d after a reset and one hand-off, want 1 — "+
			"the counter is a value the writer decides and every data node applies, "+
			"never one each node derives for itself", detail.Task.Reassignments)
	}
	if detail.Task.Assignee != after {
		t.Errorf("assignee = %q, want %q", detail.Task.Assignee, after)
	}
}

// A WRITER CANNOT ACT AS NOBODY, and the refusal reaches the CALL rather than
// the derivation.
//
// [worklet.Writer.As] is a second door into the state [worklet.NewWriter]
// guards, and a surface deriving an identity can do nothing with an error
// there, so the refusal is carried to the write; every path publishes through
// one funnel, so that is one guard rather than one per method.
func TestAWriterWithNoIdentityRefusesAtTheWrite(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("the attributed one")

	for name, w := range map[string]*worklet.Writer{
		"no author":      r.writer.As("", worklet.AuthorOperator, worklet.Provenance{}),
		"no author kind": r.writer.As("ops", "", worklet.Provenance{}),
		"a bogus kind":   r.writer.As("ops", worklet.AuthorKind("root"), worklet.Provenance{}),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.UpdateTask(t.Context(), "op-anon", task.ID, "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Title: ptr("anonymous")}, worklet.ChangeFields, nil)
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
