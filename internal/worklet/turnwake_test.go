package worklet_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// WHICH NOTICES START A TURN, REASON BY REASON.
//
// The table is the policy, written out whole so that a new reason cannot join
// [worklet.Reasons] without somebody deciding whether it is worth a turn. The
// second column is the answer when the change asks something of the seat
// holding the reason: of the task's owner, collaborators, fallback lead and
// reporter, any change but another agent's comment or a change to who
// watches it; of a watcher, a person's comment. Only those five reasons
// depend on it.
func TestEveryReasonDecidesWhetherItStartsATurn(t *testing.T) {
	t.Parallel()
	policy := map[worklet.Reason][2]bool{
		// The change was put to this seat.
		worklet.ReasonMention:     {true, true},
		worklet.ReasonPrioritised: {true, true},
		worklet.ReasonAsked:       {true, true},
		// A reply to something the seat asked.
		worklet.ReasonAnswered: {true, true},
		// Work the seat is accountable for moved.
		worklet.ReasonUnblocked:      {true, true},
		worklet.ReasonRoutedTo:       {true, true},
		worklet.ReasonParentAssignee: {true, true},
		worklet.ReasonChecklist:      {true, true},
		worklet.ReasonBlocking:       {true, true},
		worklet.ReasonPurged:         {true, true},
		// Woken when the change asks something of the seat holding it.
		worklet.ReasonAssignee:     {false, true},
		worklet.ReasonCollaborator: {false, true},
		worklet.ReasonLeadFallback: {false, true},
		worklet.ReasonReporter:     {false, true},
		worklet.ReasonWatcher:      {false, true},
		// Decided per participant: one still owing an answer in the
		// thread, or a person's comment that names nobody (ADR-0113).
		worklet.ReasonThread: {false, true},
		// A row in the inbox and never a turn: nothing is left to do.
		worklet.ReasonUnassigned: {false, false},
		worklet.ReasonUnwatched:  {false, false},
	}
	for _, r := range worklet.Reasons {
		want, ok := policy[r]
		if !ok {
			t.Errorf("%q is a reason with no decision about whether it starts a turn", r)
			continue
		}
		for i, asked := range []bool{false, true} {
			if got := r.StartsTurn(asked); got != want[i] {
				t.Errorf("%q.StartsTurn(asked=%v) = %v, want %v", r, asked, got, want[i])
			}
		}
	}
	if len(policy) != len(worklet.Reasons) {
		t.Errorf("the policy names %d reasons and the build has %d", len(policy), len(worklet.Reasons))
	}
}

// THE NOTICES A LIVE COMPANY SPENT ITS TURNS ON, AND WHAT EACH WAKES NOW.
//
// The quiet cases are changes that woke a seat into a turn which ended
// `turn_no_action` with a summary saying the change asked it nothing ("a
// status note asks nothing of QA", "an acknowledgement to backend-engineer",
// "a watcher change with no request"); the rest are changes the same
// company's seats acted on (a person's instruction, a status move by somebody
// else, a new dependency, a reassignment, a finished task to verify), which
// must still reach the seats that answer for the task.
func TestOnlyANoticeThatAsksSomethingStartsATurn(t *testing.T) {
	t.Parallel()
	agentComment := worklet.Snapshot{
		Assignee:          "backend-engineer",
		Reporter:          "pm",
		Watchers:          []string{"qa", "designer"},
		Collaborators:     []string{"app-engineer"},
		ProjectLead:       "pm-lead",
		CommentAuthorKind: worklet.AuthorAgent,
	}
	humanComment := agentComment
	humanComment.CommentAuthorKind = worklet.AuthorHuman
	// THE STATUS MOVES ACROSS THE FINISHED EDGE, recorded as a writer's
	// record carries them: the edge is told where the status moved.
	finished := map[string]worklet.Delta{"status": {From: "in_progress", To: "done"}}
	reopened := map[string]worklet.Delta{"status": {From: "done", To: "in_progress"}}

	cases := []struct {
		name  string
		n     worklet.Notify
		actor string
		want  []string
	}{{
		name:  "an agent's status note on a watched task",
		n:     worklet.Notify{Kind: worklet.ChangeComment, Snapshot: agentComment},
		actor: "sre",
		want:  nil,
	}, {
		name: "an agent's comment naming one seat and asking",
		n: worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"pm"}, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.CommentAsks, s.MentionedAgents = ptr(true), []string{"pm"}
			return s
		}()},
		actor: "sre",
		want:  []string{"pm"},
	}, {
		name: "an agent's comment naming one seat and asking nothing",
		n: worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"pm"}, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.CommentAsks, s.MentionedAgents = ptr(false), []string{"pm"}
			return s
		}()},
		actor: "sre",
		want:  nil,
	}, {
		name:  "an older build's agent comment naming one seat",
		n:     worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"pm"}, Snapshot: agentComment},
		actor: "sre",
		want:  []string{"pm"},
	}, {
		name:  "a person's comment on the task, which speaks to everyone following it",
		n:     worklet.Notify{Kind: worklet.ChangeComment, Snapshot: humanComment},
		actor: "miles",
		want:  []string{"app-engineer", "backend-engineer", "designer", "pm", "qa"},
	}, {
		name: "an agent's reply in a thread two seats wrote in",
		n: worklet.Notify{Kind: worklet.ChangeComment, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.ThreadParticipants = []string{"qa", "pm"}
			return s
		}()},
		actor: "sre",
		want:  nil,
	}, {
		name: "an agent's reply in a thread where one seat still owes an answer",
		n: worklet.Notify{Kind: worklet.ChangeComment, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.ThreadParticipants = []string{"qa", "pm"}
			s.ThreadOwing = []string{"qa"}
			return s
		}()},
		actor: "sre",
		want:  []string{"qa"},
	}, {
		name:  "a person's comment naming one seat",
		n:     worklet.Notify{Kind: worklet.ChangeComment, Mentions: []string{"pm"}, Snapshot: humanComment},
		actor: "miles",
		want:  []string{"pm"},
	}, {
		name:  "a change to who watches the task",
		n:     worklet.Notify{Kind: worklet.ChangeWatchers, Snapshot: agentComment},
		actor: "pm",
		want:  nil,
	}, {
		name:  "a status move by somebody else that neither finishes nor reopens the task",
		n:     worklet.Notify{Kind: worklet.ChangeStatus, Snapshot: agentComment},
		actor: "qa",
		want:  []string{"app-engineer", "backend-engineer"},
	}, {
		name: "somebody else finishing the seat's task",
		n: worklet.Notify{Kind: worklet.ChangeStatus, Fields: finished, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.PrevStatusGroup, s.StatusGroup = worklet.GroupActive, worklet.GroupDone
			return s
		}()},
		actor: "pm",
		want:  []string{"app-engineer", "backend-engineer"},
	}, {
		name: "a new dependent on the seat's task",
		n: worklet.Notify{Kind: worklet.ChangeRelations, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.Dependents = []worklet.TaskParty{{Task: "t-9", Assignee: "qa"}}
			return s
		}()},
		actor: "app-engineer",
		want:  []string{"backend-engineer"},
	}, {
		name: "a new assignment, which the reporter hears as the owner does",
		n: worklet.Notify{Kind: worklet.ChangeAssignee, Snapshot: agentComment,
			Fields: map[string]worklet.Delta{"assignee": {To: "backend-engineer"}}},
		actor: "miles",
		want:  []string{"app-engineer", "backend-engineer", "pm"},
	}, {
		name: "the assignee finishing the task, which its reporter verifies",
		n: worklet.Notify{Kind: worklet.ChangeStatus, Fields: finished, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.PrevStatusGroup, s.StatusGroup = worklet.GroupActive, worklet.GroupDone
			return s
		}()},
		actor: "backend-engineer",
		want:  []string{"app-engineer", "pm"},
	}, {
		name: "a reopened task",
		n: worklet.Notify{Kind: worklet.ChangeStatus, Fields: reopened, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.PrevStatusGroup, s.StatusGroup = worklet.GroupDone, worklet.GroupActive
			return s
		}()},
		actor: "qa",
		want:  []string{"app-engineer", "backend-engineer", "pm"},
	}, {
		name: "a question asked of one seat",
		n: worklet.Notify{Kind: worklet.ChangeComment, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.CommentAsk = "designer"
			return s
		}()},
		actor: "pm",
		want:  []string{"designer"},
	}, {
		name:  "an agent's comment on an unowned, unwatched task",
		n:     worklet.Notify{Kind: worklet.ChangeComment, Snapshot: worklet.Snapshot{ProjectLead: "pm", CommentAuthorKind: worklet.AuthorAgent}},
		actor: "qa",
		want:  nil,
	}, {
		name:  "a person's comment on an unowned, unwatched task",
		n:     worklet.Notify{Kind: worklet.ChangeComment, Snapshot: worklet.Snapshot{ProjectLead: "pm", CommentAuthorKind: worklet.AuthorHuman}},
		actor: "miles",
		want:  []string{"pm"},
	}, {
		name:  "a new task nobody owns",
		n:     worklet.Notify{Kind: worklet.ChangeCreated, Snapshot: worklet.Snapshot{ProjectLead: "pm"}},
		actor: "qa",
		want:  []string{"pm"},
	}, {
		name: "work routed to a unit",
		n: worklet.Notify{Kind: worklet.ChangeRouted, Snapshot: func() worklet.Snapshot {
			s := agentComment
			s.RoutedTo = "qa-lead"
			return s
		}()},
		actor: "pm",
		want:  []string{"app-engineer", "backend-engineer", "qa-lead"},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := tc.n
			got := handles(worklet.Route(worklet.Candidates(&n), everyone, tc.actor))
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("woke %v, want %v", got, want)
			}
		})
	}
}

// A REASON THAT STARTS NO TURN DOES NOT SWALLOW ONE THAT DOES.
//
// The first reason per handle is what the handle is told, and the precedence
// puts several reasons that start no turn ahead of several that do. The
// reporter who also wrote in the thread a reply lands in, and the assignee who
// also leads the unit a task was routed to, are each told the first fact and
// woken by the second.
func TestAShadowedReasonStillWakesItsHandle(t *testing.T) {
	t.Parallel()
	for name, n := range map[string]worklet.Notify{
		"a reply in the reporter's own thread": {
			Kind: worklet.ChangeComment,
			Snapshot: worklet.Snapshot{
				Reporter: "pm", ThreadParticipants: []string{"pm"},
				ThreadOwing: []string{"pm"}, CommentAuthorKind: worklet.AuthorAgent,
			},
		},
		"work routed to the assignee's own unit": {
			Kind:     worklet.ChangeRouted,
			Snapshot: worklet.Snapshot{Assignee: "qa", RoutedTo: "qa"},
		},
	} {
		cands := worklet.Candidates(&n)
		if len(cands) != 1 {
			t.Fatalf("%s: %d candidates, want the one handle once: %+v", name, len(cands), cands)
		}
		if !cands[0].Wakes {
			t.Errorf("%s: %s is told under %q and not woken", name, cands[0].Handle, cands[0].Reason)
		}
	}
}

// A NOTICE THAT STARTS NO TURN IS STILL A CANDIDATE, so the applier writes it
// to the recipient's inbox. Only the wake is withheld.
func TestANoticeThatStartsNoTurnIsStillRecorded(t *testing.T) {
	t.Parallel()
	cands := worklet.Candidates(&worklet.Notify{
		Kind: worklet.ChangeComment,
		Snapshot: worklet.Snapshot{
			Assignee: "ana", Watchers: []string{"bo"}, Reporter: "cy",
			CommentAuthorKind: worklet.AuthorAgent,
		},
	})
	for _, h := range []string{"ana", "bo", "cy"} {
		if reasonOf(cands, h) == "" {
			t.Errorf("%s is not a candidate, so the change never reaches their inbox", h)
		}
	}
	for _, c := range cands {
		if c.Wakes {
			t.Errorf("%s wakes under %q for another agent's unaddressed comment", c.Handle, c.Reason)
		}
	}
}
