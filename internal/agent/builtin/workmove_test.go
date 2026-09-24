package builtin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A CROSS-PROJECT MOVE CAN BE STARTED — by the lead of the item's project, or
// by a person.
//
// The tracker's move carried a subtree, re-keyed it, left its old keys
// resolving, marked a stopped walk and had a duty to finish one; and no tool,
// route or command could start it. The docs sent a reader to "move it" to
// clear a subtask filed in another project than its root.

// moveRegistry is a seat's registry with the move wired, and the lead lookup
// answering lead.
func moveRegistry(t *testing.T, trk *fakeTracker, lead bool) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work: builtin.WorkDeps{Reader: trk, Writer: trk.as, Moves: trk.moves},
		LeadsProject: func(context.Context, string, string) bool {
			return lead
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

func TestAMoveIsTheItemsProjectLeadsOrAPersons(t *testing.T) {
	t.Parallel()

	t.Run("a seat that does not lead the project", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		got := callWork(t, moveRegistry(t, trk, false), tracker.MoveWorkItemTool,
			map[string]any{"item": "ENG-1", "project": "OPS"})
		if !got.Failed || !strings.Contains(got.Output, "lead of ENG") {
			t.Errorf("a seat leading nothing moved an item: %s", got.Output)
		}
		if len(trk.moved) != 0 {
			t.Errorf("the refused move reached the tracker %d time(s)", len(trk.moved))
		}
	})

	t.Run("the project's lead", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := moveRegistry(t, trk, true)
		// A MOVE IS A DELIVERY: a lead woken to triage an item filed in
		// the wrong project answers by moving it, and without the flag the
		// gate corrects that turn for having done nothing.
		if got := reg.Deliveries()[tracker.MoveWorkItemTool]; got != tracker.Source {
			t.Errorf("move_work_item delivers to %q, want %q", got, tracker.Source)
		}
		got := callWork(t, reg, tracker.MoveWorkItemTool,
			map[string]any{"item": "ENG-1", "project": "ops"})
		if got.Failed {
			t.Fatalf("the lead's move failed: %s", got.Output)
		}
		answer := answerOf(t, got)
		if answer["key"] != "OPS-3" || answer["moved_from"] != "ENG-1" ||
			answer["project"] != "OPS" {
			t.Errorf("the move answered %v, want OPS-3 moved from ENG-1 into OPS", answer)
		}
		if len(trk.moved) != 1 || trk.moved[0].task != "i1" || trk.moved[0].target != "OPS" {
			t.Fatalf("the tracker was asked %+v, want item i1 into OPS — the id, "+
				"and the key upper-cased the way every project key is", trk.moved)
		}
		// AND IT WAKES THE PEOPLE ON THE ITEM, which the root's own
		// move carries.
		if n := trk.moved[0].notify; n == nil || n.Kind != tracker.ChangeMoved {
			t.Errorf("the move carries notification %+v, want a `moved` one", n)
		}
	})

	t.Run("a person", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := tools.NewRegistry()
		for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
			Work: builtin.WorkDeps{Reader: trk, Writer: trk.as, Moves: trk.moves,
				Actor: operatorActor},
		}) {
			if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
				t.Fatalf("register %s: %v", tool.Name(), err)
			}
		}
		got := callNoTurn(t, reg, tracker.MoveWorkItemTool,
			map[string]any{"item": "ENG-1", "project": "OPS"})
		if got.Failed {
			t.Fatalf("an operator's move failed: %s", got.Output)
		}
		if _, held := answerOf(t, got)["op_id"]; !held {
			t.Error("an operator's move answers no op_id to finish it under")
		}
	})
}

// A MOVE THAT STOPPED PART OF THE WAY THROUGH IS FINISHED BY THE SAME
// OPERATION, and the answer says so rather than "NOT made": the root moved.
func TestAStoppedMoveIsNotReportedAsNotMade(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = fmt.Errorf("stopped: %w", tracker.ErrStepUnresolved)
	got := callWork(t, moveRegistry(t, trk, true), tracker.MoveWorkItemTool,
		map[string]any{"item": "ENG-1", "project": "OPS"})
	if !got.Failed || strings.Contains(got.Output, "NOT made") ||
		!strings.Contains(got.Output, "exactly the same arguments") {
		t.Errorf("a stopped move gave %q", got.Output)
	}
}

// A MOVE REFUSED OVER A TAG NAMES THE TAG AND THE WAY PAST IT. The target may
// already have a different tag under the same label, or no room for another,
// and the answer was the generic "the change was NOT made" — nothing a seat
// could act on, so the move was simply abandoned.
func TestAMoveRefusedOverATagNamesTheWayPastIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want []string
	}{
		"a label the target already uses": {
			err: fmt.Errorf("tracker: declare the moving subtree's tags in OPS: %w",
				&tracker.TagClash{Project: "OPS", Slug: "api", Label: "API",
					Other: tracker.Tag{Slug: "backend-api", Label: "API"}}),
			want: []string{"nothing was written", "backend-api", "write_project on OPS",
				`"slug": "api"`, "update_work_item", "make this call again"},
		},
		"a target with no room for another tag": {
			err: fmt.Errorf("tracker: declare the moving subtree's tags in OPS: %w",
				&tracker.TagsFull{Project: "OPS", Slug: "api"}),
			want: []string{"nothing was written", "tags_archive", "take api off",
				"make this call again"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.writeErr = tc.err
			got := callWork(t, moveRegistry(t, trk, true), tracker.MoveWorkItemTool,
				map[string]any{"item": "ENG-1", "project": "OPS"})
			if !got.Failed || strings.Contains(got.Output, "NOT made") {
				t.Fatalf("the refused move answered %q", got.Output)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the answer lacks %q: %s", want, got.Output)
				}
			}
		})
	}
}

// landingMover is a mover whose first move LANDS and whose answer is lost —
// unknown, or a walk stopped after the root moved — and which then answers
// the way the tracker's ledger does: this operation's own move, applied, and
// any other operation's refused as somebody else's.
type landingMover struct {
	trk   *fakeTracker
	first func(opID string) (tracker.WriteResult, error)
	ops   []string
}

func (m *landingMover) MoveTaskToProject(_ context.Context, opID, _, target string,
	_ *tracker.Notify) (tracker.WriteResult, error) {

	m.ops = append(m.ops, opID)
	if len(m.ops) == 1 {
		// THE ROOT MOVES: re-keyed in the target, its old key still
		// resolving to it (the fake's map is keyed on the key it was
		// filed under).
		moved := m.trk.tasks["ENG-1"]
		moved.Task.FormerKeys = []string{moved.Task.Key}
		moved.Task.Key, moved.Task.Project = target+"-3", target
		m.trk.tasks["ENG-1"] = moved
		return m.first(opID)
	}
	if opID != m.ops[0] {
		return tracker.WriteResult{}, fmt.Errorf("tracker: task i1 is already in "+
			"%s, and this node's operation ledger holds no record of this move "+
			"putting it there", target)
	}
	return tracker.WriteResult{Key: target + "-3", Result: statelog.Result{
		Outcome: statelog.OutcomeApplied, OpID: opID, Collapsed: true,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 71},
		Version:  71,
	}}, nil
}

// THE RETRY A MOVE PRESCRIBES IS ANSWERED BY THE MOVE, not refused by the gate
// the first attempt already passed.
//
// The gate asked about the lead of the project the item is in NOW, and after
// a first attempt that landed that is the TARGET: a seat leading ENG and not
// OPS, told to call again with the same arguments, was told instead that
// moving OPS-3 out of OPS was OPS's lead's decision — about a move that had
// happened, which it never learned.
func TestTheRetryOfAMoveThatLandedIsAnsweredByTheMove(t *testing.T) {
	t.Parallel()
	for name, first := range map[string]func(string) (tracker.WriteResult, error){
		"an unknown": func(opID string) (tracker.WriteResult, error) {
			return tracker.WriteResult{Result: statelog.Result{
				Outcome: statelog.OutcomeUnknown, OpID: opID,
			}}, nil
		},
		"a walk stopped after the root moved": func(string) (tracker.WriteResult, error) {
			return tracker.WriteResult{}, fmt.Errorf("tracker: whether task "+
				"id-9's move into OPS landed is unknown: %w", tracker.ErrStepUnresolved)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			mover := &landingMover{trk: trk, first: first}
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, builtin.Deps{
				Work: builtin.WorkDeps{Reader: trk, Writer: trk.as,
					Moves: func(builtin.Actor) builtin.WorkMover { return mover }},
				// THE SEAT LEADS THE SOURCE AND NOT THE TARGET.
				LeadsProject: func(_ context.Context, _, project string) bool {
					return project == "ENG"
				},
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			args := map[string]any{"item": "ENG-1", "project": "OPS"}

			got := callWork(t, reg, tracker.MoveWorkItemTool, args)
			if !got.Failed || !strings.Contains(got.Output, "exactly the same arguments") {
				t.Fatalf("the first attempt answered %q, want the retry it prescribes",
					got.Output)
			}
			retry := callWork(t, reg, tracker.MoveWorkItemTool, args)
			if retry.Failed {
				t.Fatalf("the prescribed retry was refused: %s", retry.Output)
			}
			if len(mover.ops) != 2 || mover.ops[0] != mover.ops[1] {
				t.Fatalf("the mover was asked under %q, want the same operation "+
					"twice", mover.ops)
			}
			answer := answerOf(t, retry)
			// MOVED FROM THE KEY THE MOVE REPLACED, not the one it minted.
			if answer["key"] != "OPS-3" || answer["moved_from"] != "ENG-1" ||
				answer["project"] != "OPS" {
				t.Errorf("the retry answered %v, want OPS-3 moved from ENG-1 into OPS",
					answer)
			}
		})
	}
}

// A MOVE INTO THE PROJECT AN ITEM IS ALREADY IN THAT NO EARLIER ATTEMPT MADE is
// the tracker's to refuse, and it does — so passing the gate there hands a seat
// nothing: the answer is the refusal, and says the change was not made.
func TestAMoveIntoTheItemsOwnProjectIsTheTrackersToRefuse(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	mover := &landingMover{trk: trk}
	mover.ops = []string{"somebody else's move"}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work: builtin.WorkDeps{Reader: trk, Writer: trk.as,
			Moves: func(builtin.Actor) builtin.WorkMover { return mover }},
		LeadsProject: func(context.Context, string, string) bool { return false },
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got := callWork(t, reg, tracker.MoveWorkItemTool,
		map[string]any{"item": "ENG-1", "project": "ENG"})
	if !got.Failed || !strings.Contains(got.Output, "no record of this move") ||
		!strings.Contains(got.Output, "NOT made") {
		t.Errorf("a move into the item's own project answered %q, want the "+
			"tracker's refusal", got.Output)
	}
}
