package tracker_test

import (
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A TICK PUBLISHES ONE RECORD PER SUBJECT, NOT ONE PER EDGE.
//
// # The invariant, and what breaks without it
//
// The subject is the arbitration unit: a second record on it cannot be decided
// until this node has applied the first, and the write path refuses the
// attempt as `behind` rather than waiting indefinitely. So a repair that
// published one record per edge would land the first edge behind a blocker and
// be refused on every edge after it — and edges sharing a blocker are the
// ordinary shape, because the dependent's own commit lands first and it is the
// blocker's mirror that loses the race.
//
// PURE, over values the scan already read, because that is where the grouping
// decision lives — [tracker.PlanOneSided] — and a rule only reachable through
// a broker and a store is a rule nobody re-measures.
func TestAMirrorRepairPlansOneCommitPerSubject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		edges []tracker.OneSided
		want  []tracker.OneSidedCommit
	}{
		{
			name: "edges sharing a blocker collapse into one commit",
			edges: []tracker.OneSided{
				brokenEdge("d-1", "blk"),
				brokenEdge("d-2", "blk"),
				brokenEdge("d-3", "blk"),
			},
			want: []tracker.OneSidedCommit{{
				Task: "blk", Project: "ENG",
				Mirror: []tracker.OneSided{
					brokenEdge("d-1", "blk"),
					brokenEdge("d-2", "blk"),
					brokenEdge("d-3", "blk"),
				},
			}},
		},
		{
			name: "edges on different blockers stay separate commits",
			edges: []tracker.OneSided{
				brokenEdge("d-1", "blk-a"),
				brokenEdge("d-2", "blk-b"),
			},
			want: []tracker.OneSidedCommit{
				{Task: "blk-a", Project: "ENG",
					Mirror: []tracker.OneSided{brokenEdge("d-1", "blk-a")}},
				{Task: "blk-b", Project: "ENG",
					Mirror: []tracker.OneSided{brokenEdge("d-2", "blk-b")}},
			},
		},
		{
			// THE CASE THAT NEEDS BOTH HALVES ON ONE RECORD: a chain
			// a → x → y where both edges are broken and y is gone. The
			// mirror lands on x's subject because x is a's blocker, and
			// the stamp lands on x's subject because x is the dependent
			// that authored the edge to y.
			name: "a task that is both a blocker and a stamped dependent takes one commit",
			edges: []tracker.OneSided{
				brokenEdge("a", "x"),
				missingBlockerEdge("x", "y"),
			},
			want: []tracker.OneSidedCommit{{
				Task: "x", Project: "ENG",
				Mirror: []tracker.OneSided{brokenEdge("a", "x")},
				Final:  []tracker.OneSided{missingBlockerEdge("x", "y")},
			}},
		},
		{
			// THE STAMP IS ON THE DEPENDENT, so two unmirrorable edges
			// authored by ONE task are one commit and not two — the
			// same contention, on the other side of the edge.
			name: "unmirrorable edges from one dependent take one commit",
			edges: []tracker.OneSided{
				missingBlockerEdge("d", "gone-1"),
				missingBlockerEdge("d", "gone-2"),
			},
			want: []tracker.OneSidedCommit{{
				Task: "d", Project: "ENG",
				Final: []tracker.OneSided{
					missingBlockerEdge("d", "gone-1"),
					missingBlockerEdge("d", "gone-2"),
				},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tracker.PlanOneSided(tc.edges)
			if !sameCommits(got, tc.want) {
				t.Fatalf("%d edges planned into %s, want %s",
					len(tc.edges), showCommits(got), showCommits(tc.want))
			}
		})
	}
}

// A BLOCKER TAKES ONLY WHAT IT HAS ROOM FOR, and the rest are left rather than
// refused with them.
//
// [tracker.OneSided.Final] answers "is this blocker full ALREADY", which is a
// property of the blocker and therefore the same for every edge behind it. It
// does not answer "will it be full once this tick adds these" — so a blocker
// with room for one that three edges name would, unsplit, produce a commit
// `checkDependents` refuses inside the decide. That refusal takes the edge
// that fitted down with the two that did not, on this sweep and on every sweep
// after it, because nothing about the rows changes between them.
//
// So the plan admits exactly the room and leaves the rest where they are: the
// next sweep reads the blocker at its cap, [tracker.OneSided.Final] is true
// for them, and they are stamped rather than retried for ever.
func TestAMirrorRepairPlansNoMoreThanABlockerHasRoomFor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		already int
		edges   int
		want    int
	}{
		{name: "an empty blocker takes every edge", already: 0, edges: 3, want: 3},
		{name: "room for one takes one", already: tracker.MaxDependents - 1,
			edges: 3, want: 1},
		{name: "room for two takes two", already: tracker.MaxDependents - 2,
			edges: 5, want: 2},
		{
			// AT THE CAP THE EDGES ARE FINAL, not deferred: the plan
			// stamps them on their own dependents rather than leaving
			// a tick with nothing to do for ever.
			name: "a full blocker stamps instead", already: tracker.MaxDependents,
			edges: 2, want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var edges []tracker.OneSided
			for i := range tc.edges {
				edge := brokenEdge(fmt.Sprintf("d-%d", i), "blk")
				edge.BlockerDependents = tc.already
				edges = append(edges, edge)
			}
			var mirrored int
			for _, commit := range tracker.PlanOneSided(edges) {
				mirrored += len(commit.Mirror)
			}
			if mirrored != tc.want {
				t.Errorf("a blocker holding %d of %d took %d of %d edges, want "+
					"%d — over the cap the whole commit is refused inside the "+
					"decide and the edges that fitted are lost with it",
					tc.already, tracker.MaxDependents, mirrored, tc.edges, tc.want)
			}
			// WHAT IS LEFT IS WHAT THE DUTY REPORTS AS DEFERRED, which
			// is the subtraction its log line makes: every edge the
			// scan read that this tick does not carry.
			if left := tc.edges - mirrored; left < 0 {
				t.Fatalf("the plan carried %d of %d edges", mirrored, tc.edges)
			}
		})
	}
}

// brokenEdge is one repairable edge: the blocker is present, open and has
// room, so [tracker.OneSided.Final] is false for it.
func brokenEdge(dependent, blocker string) tracker.OneSided {
	return tracker.OneSided{
		Dependent: dependent, Blocker: blocker,
		DependentKey: "ENG-1", DependentProject: "ENG", DependentAssignee: "alice",
		BlockerProject: "ENG", BlockerKey: "ENG-2", BlockerAssignee: "bob",
	}
}

// missingBlockerEdge is one edge whose mirror can never be written, which is
// what puts it on the DEPENDENT's subject rather than the blocker's.
func missingBlockerEdge(dependent, blocker string) tracker.OneSided {
	edge := brokenEdge(dependent, blocker)
	edge.BlockerMissing = true
	edge.BlockerProject, edge.BlockerKey, edge.BlockerAssignee = "", "", ""
	return edge
}

func sameCommits(got, want []tracker.OneSidedCommit) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Task != want[i].Task || got[i].Project != want[i].Project {
			return false
		}
		if !sameEdges(got[i].Mirror, want[i].Mirror) ||
			!sameEdges(got[i].Final, want[i].Final) {
			return false
		}
	}
	return true
}

func sameEdges(got, want []tracker.OneSided) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func showCommits(commits []tracker.OneSidedCommit) string {
	out := ""
	for _, c := range commits {
		mirror := make([]string, 0, len(c.Mirror))
		for _, e := range c.Mirror {
			mirror = append(mirror, e.Dependent)
		}
		final := make([]string, 0, len(c.Final))
		for _, e := range c.Final {
			final = append(final, e.Blocker)
		}
		out += fmt.Sprintf("{on %s mirror=%v final-on=%v}", c.Task, mirror, final)
	}
	if out == "" {
		return "no commits"
	}
	return out
}
