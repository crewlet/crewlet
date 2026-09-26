package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// TWO CALLS IN ONE TURN ARE TWO WRITES, AND A REDELIVERY NAMES BOTH AGAIN.
//
// Every write carries an operation id, and the tracker's ledger answers a
// write under an id it already applied `applied` without writing it — which is
// what collapses a redelivered turn into its first attempt, and what drops a
// second write in one turn named like the first. These cases drive the tools
// through the surface a phase runs them on, which is the frame that hands each
// call the calls before it; the ids are the property, and the ledger that
// collapses equal ones is certified in the tracker's own suite.

// attemptOf is one run of the unit of work "wk-1": two runs of it are a
// redelivery.
func attemptOf(t *testing.T, runID string) *turnctx.Turn {
	t.Helper()
	turn := workTurn(t)
	turn.RunID, turn.WorkKey = runID, "wk-1"
	return turn
}

// onSurface offers every tool reg holds on one surface bound to turn.
func onSurface(reg *tools.Registry, turn *turnctx.Turn) *tools.Surface {
	snap := reg.Snapshot()
	return tools.NewSurface("execute", snap, snap.Names()).ForTurn(turn)
}

// operatorToolsOver registers the operator catalogue built over work.
func operatorToolsOver(t *testing.T, work builtin.WorkDeps) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Work: work}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

func execute(t *testing.T, s *tools.Surface, name string, args map[string]any) toolloop.ToolResult {
	t.Helper()
	got, err := s.Execute(t.Context(), llm.ToolCall{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// Two updates to one item in one turn — the second naming it by id where the
// first named it by key, because the count is of the object a write resolved
// rather than of what was typed.
//
// Mutation: name every write its base, or hand the tool the bound turn rather
// than the calls before it, and the second update is named like the first.
func TestTwoUpdatesToOneItemInOneTurnAreTwoWrites(t *testing.T) {
	t.Parallel()
	named := func(runID string) []string {
		trk := newFakeTracker()
		s := onSurface(workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
			attemptOf(t, runID))
		for _, args := range []map[string]any{
			{"item": "ENG-1", "priority": "urgent"},
			{"item": "i1", "priority": "low"},
		} {
			if got := execute(t, s, builtin.UpdateWorkItemTool, args); got.Failed {
				t.Fatalf("update failed: %s", got.Output)
			}
		}
		return trk.opIDs
	}
	first := named("run-1")
	// The first is the name a build that does not count gives every write
	// to this item in the turn, which is what keeps a redelivery across a
	// rolling upgrade from landing it twice.
	if want := []string{"wk-1-update-i1", "wk-1-update-i1#2"}; !slices.Equal(first, want) {
		t.Errorf("two updates in one turn wrote under %v, want %v — equal names "+
			"are one write, and the second is answered applied and dropped", first, want)
	}
	if again := named("run-2"); !slices.Equal(again, first) {
		t.Errorf("the redelivery wrote under %v where its first attempt wrote "+
			"under %v — each write lands twice", again, first)
	}
}

// A WRITE THAT NAMED ITS OPERATION AND DID NOT LAND IS STILL COUNTED.
//
// Its record may have landed on an earlier attempt that lost the answer, and a
// redelivery of that attempt names its next write after it — so this attempt
// must too, or the next write is named like the one the ledger holds.
//
// Mutation: answer a failed write without its operations and this fails.
func TestAWriteThatNamedItsOperationAndDidNotLandIsCounted(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	s := onSurface(workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
		attemptOf(t, "run-1"))
	trk.writeErr = errors.New("no response from stream")
	if got := execute(t, s, builtin.UpdateWorkItemTool,
		map[string]any{"item": "ENG-1", "priority": "urgent"}); !got.Failed {
		t.Fatalf("the failing update reported success: %s", got.Output)
	}
	trk.writeErr = nil
	if got := execute(t, s, builtin.UpdateWorkItemTool,
		map[string]any{"item": "ENG-1", "priority": "low"}); got.Failed {
		t.Fatalf("update failed: %s", got.Output)
	}
	if want := []string{"wk-1-update-i1#2"}; !slices.Equal(trk.opIDs, want) {
		t.Errorf("the write after a failed one landed under %v, want %v", trk.opIDs, want)
	}
}

// TWO COMMENTS ON ONE ITEM IN ONE TURN ARE TWO COMMENTS, each with an id of its
// own — the id is a primary key on every node — and a redelivery posts neither
// again.
func TestTwoCommentsOnOneItemInOneTurnAreTwoComments(t *testing.T) {
	t.Parallel()
	posted := func(runID string) (ids, ops []string) {
		trk := newFakeTracker()
		s := onSurface(workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
			attemptOf(t, runID))
		for _, body := range []string{"Found the cause.", "Fixed it."} {
			if got := execute(t, s, builtin.CommentOnWorkTool,
				map[string]any{"item": "ENG-1", "body": body}); got.Failed {
				t.Fatalf("comment failed: %s", got.Output)
			}
		}
		for _, p := range trk.patched {
			ids = append(ids, p.Comment.ID)
		}
		return ids, trk.opIDs
	}
	ids, ops := posted("run-1")
	if len(ids) != 2 || ids[0] == ids[1] || ops[0] == ops[1] {
		t.Fatalf("two comments were posted as %v under %v — one comment, and "+
			"the second dropped", ids, ops)
	}
	if againIDs, againOps := posted("run-2"); !slices.Equal(againIDs, ids) ||
		!slices.Equal(againOps, ops) {
		t.Errorf("the redelivery posted %v under %v where its first attempt "+
			"posted %v under %v — every comment twice", againIDs, againOps, ids, ops)
	}
}

// TWO CREATES IN ONE TURN FILE TWO TASKS, AND A REDELIVERY FILES THE SAME TWO.
//
// A task's id is derived from the turn and the create's place among its
// creates: drawn at random, a redelivered turn files every task a second time,
// and nothing the ledger holds can collapse it.
//
// Mutation: draw the id at random and the redelivery files new tasks.
func TestTwoCreatesInOneTurnFileTwoTasksAndARedeliveryTheSameTwo(t *testing.T) {
	t.Parallel()
	filed := func(runID string) (ids, ops []string) {
		trk := newFakeTracker()
		s := onSurface(workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
			attemptOf(t, runID))
		for range 2 {
			// THE SAME ARGUMENTS BOTH TIMES: two follow-ups a model
			// titled alike are still two.
			if got := execute(t, s, builtin.CreateWorkItemTool,
				map[string]any{"title": "follow up", "project": "ENG"}); got.Failed {
				t.Fatalf("create failed: %s", got.Output)
			}
		}
		for _, task := range trk.created {
			ids = append(ids, task.ID)
		}
		return ids, trk.opIDs
	}
	ids, ops := filed("run-1")
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("two creates filed %v", ids)
	}
	for i, op := range ops {
		if op != "wk-1-create-"+ids[i] {
			t.Errorf("create %d was filed under %q, want wk-1-create-%s", i+1, op, ids[i])
		}
	}
	if again, _ := filed("run-2"); !slices.Equal(again, ids) {
		t.Errorf("the redelivery filed %v where its first attempt filed %v — "+
			"the same work, filed twice", again, ids)
	}
}

// EVERY WRITE IS STAMPED WITH THE INSTANT ITS TURN'S IDS CAN FIRST HAVE BEEN
// MINTED, and an operator's with none.
//
// A derived id is minted by the first attempt that wrote under it, and a node
// that adopted a snapshot since answers a write stamped before the adoption
// from what the adoption brought — while one stamped with a retry's own call
// reads as newer and is decided a second time.
//
// Mutation: leave MintedAt out of the actor a turn writes as and this fails.
func TestAWriteCarriesItsTurnsMintInstant(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 26, 8, 30, 0, 0, time.UTC)
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	turn := attemptOf(t, "run-1")
	turn.TriggeredAt = at
	if got := execute(t, onSurface(reg, turn), builtin.UpdateWorkItemTool,
		map[string]any{"item": "ENG-1", "priority": "urgent"}); got.Failed {
		t.Fatalf("update failed: %s", got.Output)
	}
	if len(trk.actors) != 1 || !trk.actors[0].MintedAt.Equal(at) {
		t.Fatalf("the writer was derived for %+v, want MintedAt %v", trk.actors, at)
	}

	// AN OPERATOR'S ids are fresh, minted by the call that carries them.
	operator := newFakeTracker()
	ops := workRegistry(t, builtin.WorkDeps{
		Reader: operator, Writer: operator.as, Actor: operatorActor,
	})
	if got := callNoTurn(t, ops, builtin.UpdateWorkItemTool,
		map[string]any{"item": "ENG-1", "priority": "urgent"}); got.Failed {
		t.Fatalf("update failed: %s", got.Output)
	}
	if len(operator.actors) != 1 || !operator.actors[0].MintedAt.IsZero() {
		t.Errorf("an operator's writer was derived for %+v, want no mint instant",
			operator.actors)
	}
}

// ---- the trash --------------------------------------------------------- //

// fakeTrash records the trash writes, and answers each with the next of errs.
type fakeTrash struct {
	verbs, ops []string
	errs       []error
}

func (f *fakeTrash) answer(verb, opID string) (tracker.WriteResult, error) {
	f.verbs, f.ops = append(f.verbs, verb), append(f.ops, opID)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return tracker.WriteResult{}, err
		}
	}
	return tracker.WriteResult{Result: statelog.Result{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 61},
	}}, nil
}

func (f *fakeTrash) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return f.answer("remove", opID)
}

func (f *fakeTrash) RestoreTask(_ context.Context, opID, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return f.answer("restore", opID)
}

// trashSurface is the operator catalogue over one fake tracker and trash, on a
// surface bound to a turn.
func trashSurface(t *testing.T, trk *fakeTracker, trash *fakeTrash) *tools.Surface {
	t.Helper()
	reg := operatorToolsOver(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as,
		TrashWriter: func(builtin.Actor) builtin.TrashWriter { return trash },
	})
	return onSurface(reg, attemptOf(t, "run-1"))
}

// A RESTORE THAT STOPPED PART-WAY IS FINISHED BY CALLING IT AGAIN.
//
// A restore walks what the removal took after bringing the root back, so one
// stopped part-way leaves its root out of the trash and some of what went
// with it still in. Its refusal says so and says to call it again — which the
// tool must then do rather than refuse a root that is no longer in the trash,
// or nothing short of a hand-written walk ever finishes it. And the second
// call is a second write, named apart from the first.
//
// Mutation: refuse a restore whose root is not in the trash, or answer the
// partial refusal as a change that was not made, and this fails.
func TestARestoreThatStoppedPartWayIsFinishedByCallingItAgain(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker() // ENG-1 is live: its restore's root step landed
	trash := &fakeTrash{errs: []error{&tracker.PartialError{Rerun: true,
		Err: errors.New("tracker: i1 is out of the trash and 1 of 3 tasks " +
			"removed with it followed; re-run the restore to finish, which " +
			"is idempotent: conflict")}}}
	s := trashSurface(t, trk, trash)

	first := execute(t, s, tracker.RestoreWorkItemTool, map[string]any{"item": "ENG-1"})
	if !first.Failed || !strings.Contains(first.Output, "WAS made") ||
		!strings.Contains(first.Output, "Call restore_work_item again") ||
		strings.Contains(first.Output, "NOT made") {
		t.Errorf("a restore that stopped part-way answered %q — a caller told the "+
			"change was not made reports a state that is not true", first.Output)
	}
	second := execute(t, s, tracker.RestoreWorkItemTool, map[string]any{"item": "ENG-1"})
	if second.Failed {
		t.Fatalf("the restore that finishes the first was refused: %s", second.Output)
	}
	if want := []string{"wk-1-restore-i1", "wk-1-restore-i1#2"}; !slices.Equal(trash.ops, want) {
		t.Errorf("the two restores wrote under %v, want %v — the second named "+
			"like the first is answered applied and finishes nothing", trash.ops, want)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(second.Output), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %s", second.Output)
	}
	if note, _ := answer["note"].(string); !strings.Contains(note, "was not in the trash") {
		t.Errorf("a restore of a live item says nothing about it: %s", second.Output)
	}
}

// A REMOVAL AFTER A RESTORE IN ONE TURN IS A REMOVAL, named apart from the
// first: same verb, same item, two writes.
func TestARemovalAfterARestoreInOneTurnIsARemoval(t *testing.T) {
	t.Parallel()
	trk, trash := newFakeTracker(), &fakeTrash{}
	s := trashSurface(t, trk, trash)
	for _, name := range []string{tracker.RemoveWorkItemTool,
		tracker.RestoreWorkItemTool, tracker.RemoveWorkItemTool} {
		if got := execute(t, s, name, map[string]any{"item": "ENG-1"}); got.Failed {
			t.Fatalf("%s failed: %s", name, got.Output)
		}
	}
	want := []string{"wk-1-remove-i1", "wk-1-restore-i1", "wk-1-remove-i1#2"}
	if !slices.Equal(trash.ops, want) {
		t.Errorf("remove, restore, remove wrote under %v, want %v", trash.ops, want)
	}
}

// A GESTURE WHOSE FIRST COMMIT LANDED IS NOT ANSWERED AS ONE THAT DID NOTHING,
// whichever way it finishes — and a failure that wrote nothing still is.
//
// Mutation: drop the partial arm from the write failure and every one of these
// reads as a change that was not made.
func TestAPartialGestureIsNotAnsweredAsOneThatDidNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		err       error
		wantParts []string
		notParts  []string
	}{
		{"a removal a second call finishes",
			&tracker.PartialError{Rerun: true, Err: errors.New("the root is in the trash")},
			[]string{"WAS made", "Call remove_work_item again"},
			[]string{"NOT made"}},
		{"one whose rest another path finishes",
			&tracker.PartialError{Err: errors.New("the tracker duty completes it")},
			[]string{"WAS made", "does not finish it", "the tracker duty completes it"},
			[]string{"NOT made", "Call remove_work_item again"}},
		{"wrapped, as a caller above the tracker returns it",
			errors.Join(errors.New("while removing"),
				&tracker.PartialError{Rerun: true, Err: errors.New("the root is in the trash")}),
			[]string{"WAS made"},
			[]string{"NOT made"}},
		{"one that wrote nothing", errors.New("the root was refused"),
			[]string{"NOT made"}, []string{"WAS made"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trash := &fakeTrash{errs: []error{tc.err}}
			got := execute(t, trashSurface(t, newFakeTracker(), trash),
				tracker.RemoveWorkItemTool, map[string]any{"item": "ENG-1"})
			if !got.Failed {
				t.Fatalf("the removal reported success: %s", got.Output)
			}
			for _, part := range tc.wantParts {
				if !strings.Contains(got.Output, part) {
					t.Errorf("the answer %q does not say %q", got.Output, part)
				}
			}
			for _, part := range tc.notParts {
				if strings.Contains(got.Output, part) {
					t.Errorf("the answer %q says %q", got.Output, part)
				}
			}
		})
	}
}
