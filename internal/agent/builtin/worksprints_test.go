package builtin_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SPRINT NOBODY MINTED IS MISSING, NOT UNAVAILABLE.
//
// # What was wrong
//
// `manage_sprint` published its record and let the writer's decide refuse.
// That decide reads this node's own rows inside one transaction, and an absent
// row there is genuinely ambiguous — a sprint no record ever minted and one
// this node has not applied yet look identical — so it said the second:
//
//	tracker: sprint ENG.9 is not on this node: statelog: unavailable
//
// wrapping [statelog.ErrUnavailable], the sentinel whose whole contract is
// "this may work later, or elsewhere". Measured against a running engine,
// `start` on a sprint number nobody had minted answered exactly that, and no
// retry on any node would ever have made it true. The tool one over answers
// `There is no work item "ENG-999".` for the same shape of mistake.
//
// # Why a read can say what the decide cannot
//
// A read is served at a level that waits for this node to be caught up, and it
// reports its COVERAGE separately from its freshness — so complete-and-empty
// is a definitive absence, and incomplete is the third value said out loud
// rather than collapsed into either of the other two. That is the same rule
// `internal/coord` states for ownership: held, definitively not, and unknown
// are three answers, and folding the last into one of the first two is how a
// caller is told to retry something that cannot succeed, or told a thing does
// not exist when nobody knows.
//
// # What is asserted
//
// The REFUSAL and the WRITER, together. A refusal that reads correctly while
// the record still goes out is a durable write nothing applies; a writer left
// untouched while the refusal invites a retry is the original bug wearing
// different words. So every case names both, and the last one is the control —
// without it a tool that refused everything would pass all three.

// trackerHoldingSprint is a reader for which that sprint exists, completely.
func trackerHoldingSprint(project string, number int) *fakeTracker {
	trk := newFakeTracker()
	trk.sprints = tracker.SprintListing{
		Project:  project,
		Sprints:  []tracker.SprintRow{{Project: project, Number: number, State: tracker.SprintFuture}},
		Level:    statelog.ReadLinearizable,
		Complete: true,
	}
	return trk
}

// sprintTools is the operator surface over one reader and one spy, acting as a
// PERSON — the actor `manage_sprint` exists for, so the lead lookup is not
// what any of these cases is about.
func sprintTools(t *testing.T, reader *fakeTracker, spy *sprintSpy) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:       reader,
			Writer:       newFakeTracker().as,
			SprintWriter: func(builtin.Actor) builtin.SprintWriter { return spy },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "founder", Kind: tracker.AuthorOperator}, nil
			},
		},
		LeadsProject: func(context.Context, string, string) bool { return false },
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

func TestASprintNobodyMintedIsRefusedAsMissingRatherThanUnavailable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reader  func() *fakeTracker
		want    []string
		notWant []string
		started int
	}{
		// COMPLETE AND EMPTY: the read covered every row that could
		// match and found none, so the sprint does not exist.
		"a sprint nobody minted": {
			reader: func() *fakeTracker {
				trk := newFakeTracker()
				trk.sprints = tracker.SprintListing{
					Project: "ENG", Level: statelog.ReadLinearizable, Complete: true,
				}
				return trk
			},
			want: []string{"no sprint 9", "ENG", "sprint_report"},
			// NEITHER HALF OF THE OLD ANSWER: not the phrase that
			// sends a reader looking at another node, and not the
			// sentinel that tells a surface to retry.
			notWant: []string{"not on this node", "unavailable"},
		},
		// INCOMPLETE: this node holds records it cannot read whose
		// scope reaches this question, so nobody knows.
		"a node that cannot account for its own records": {
			reader: func() *fakeTracker {
				trk := newFakeTracker()
				trk.sprints = tracker.SprintListing{
					Project: "ENG", Level: statelog.ReadLinearizable,
					Complete:   false,
					Incomplete: &tracker.Incomplete{Records: 3},
				}
				return trk
			},
			want: []string{"could not", "3 record"},
			// AND IT MUST NOT CLAIM ABSENCE, which is the failure
			// in the other direction and the worse one: a lead told
			// their sprint does not exist goes and mints another.
			notWant: []string{"There is no sprint"},
		},
		"a project nobody created": {
			reader: func() *fakeTracker {
				trk := newFakeTracker()
				trk.readErr = fmt.Errorf("tracker: no project %q — %w", "ENG",
					tracker.ErrNoProject)
				return trk
			},
			want:    []string{"no project", "list_projects"},
			notWant: []string{"not on this node"},
		},
		// THE CONTROL. The resolution is a precondition, not a veto.
		"a sprint that is there": {
			reader:  func() *fakeTracker { return trackerHoldingSprint("ENG", 9) },
			started: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			spy := &sprintSpy{}
			got := callWork(t, sprintTools(t, tc.reader(), spy),
				tracker.ManageSprintTool, map[string]any{
					"project": "ENG", "action": "start", "sprint": 9,
				})
			if spy.started != tc.started {
				t.Fatalf("the sprint was started %d times, want %d — %q",
					spy.started, tc.started, got.Output)
			}
			if tc.started > 0 {
				if got.Failed {
					t.Fatalf("a sprint that exists was refused: %q", got.Output)
				}
				return
			}
			if !got.Failed {
				t.Fatalf("the call did not fail: %q", got.Output)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the refusal does not say %q: %q", want, got.Output)
				}
			}
			for _, never := range tc.notWant {
				if strings.Contains(got.Output, never) {
					t.Errorf("the refusal still says %q, which sends the "+
						"caller to retry something that cannot succeed: %q",
						never, got.Output)
				}
			}
		})
	}
}

// AND THE RESOLUTION IS LINEARIZABLE AND INCLUDES THE ARCHIVED.
//
// The level because a stale read answers "there is no sprint 9" about a sprint
// minted a second ago — which is the original bug with the sign flipped, and
// the one a fix in a hurry ships. Archived because an archived sprint EXISTS:
// refused here as missing, a lead would be told to mint a sprint that is
// already there, and what an archived one may become is the record's own state
// machine to say, in its own words.
func TestTheSprintResolutionAsksForWhatItHasToSee(t *testing.T) {
	t.Parallel()
	reader := trackerHoldingSprint("ENG", 4)
	callWork(t, sprintTools(t, reader, &sprintSpy{}), tracker.ManageSprintTool,
		map[string]any{"project": "ENG", "action": "close", "sprint": 4})
	if got := reader.sprintQuery.Level; got != statelog.ReadLinearizable {
		t.Errorf("the sprint was resolved at %q — a stale read answers "+
			"\"there is no sprint 4\" about one minted a moment ago",
			got)
	}
	if !reader.sprintQuery.Archived {
		t.Error("the resolution skips the archived sprints, so an archived " +
			"one is refused as a sprint that was never minted")
	}
	if reader.sprintQuery.Number != 4 || reader.sprintQuery.Project != "ENG" {
		t.Errorf("the resolution asked for %s.%d rather than ENG.4",
			reader.sprintQuery.Project, reader.sprintQuery.Number)
	}
}

// A LIST'S ROWS ARE THE ROWS THAT MATCH.
//
// # What was wrong
//
// The query grammar's default subtask mode is `collapsed`, where the filter is
// a predicate on the ROOT task and its whole subtree rides along UNFILTERED.
// That is right for a board, which draws a tree; it is wrong for an answer a
// model reads as a list, and nothing on the answer says which rows matched and
// which came for the ride.
//
// Measured against a running engine: `list_work_items assignee=agent-cto`
// answered nine rows, six of them assigned to `backend-engineer` — the
// subtasks of an epic the CTO owns — while `assignee=backend-engineer`
// answered three, a DISJOINT set that did not include any of the six. So a
// seat asking what is on its plate counted somebody else's work, and the same
// seat's own query could not find it.
//
// # Why overruled rather than defaulted
//
// One grammar serves the board, the socket, the REST route and this tool, so a
// saved `view` reaches this call carrying an answer shape somebody chose for a
// board. A tree is board furniture, which this tool already declines along
// with `group_by` and `totals`. The surface is the authority, exactly as it is
// for the read level one line above.
func TestAListsRowsAreTheRowsThatMatch(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"a plain filter":       {"assignee": "ada"},
		"a saved view's shape": {"view": "a-board-someone-saved"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			callWork(t, reg, builtin.ListWorkItemsTool, args)
			if got := trk.query.Subtasks; got != tracker.SubtasksSeparate {
				t.Errorf("the list asked for subtasks=%q — under any other mode "+
					"the filter is a predicate on the ROOT and its whole "+
					"subtree rides along unfiltered, so the answer carries "+
					"rows that do not match and says nothing about which", got)
			}
		})
	}
}
