package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/worklet"
)

// everyone is a registry that employs anybody.
func everyone(string) bool { return true }

// reasonOf finds the reason a handle appears under, or "".
func reasonOf(cands []routing.Candidate[worklet.Reason], handle string) worklet.Reason {
	for _, c := range cands {
		if c.Handle == handle {
			return c.Reason
		}
	}
	return ""
}

func handles(cands []routing.Candidate[worklet.Reason]) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Handle)
	}
	slices.Sort(out)
	return out
}

// A QUIET COMMIT CONCERNS NOBODY, ON BOTH SURFACES: the feed never routes
// one, and the applier, calling this same function, writes no row for it.
func TestAQuietCommitConcernsNobody(t *testing.T) {
	t.Parallel()
	if got := worklet.Candidates(nil); got != nil {
		t.Fatalf("a nil notification produced %d candidate(s)", len(got))
	}
}

// THE FIRST REASON PER HANDLE WINS: mentioned AND watching is told
// "mentioned", the stronger fact.
func TestTheFirstReasonPerHandleWins(t *testing.T) {
	t.Parallel()
	cands := worklet.Candidates(&worklet.Notify{
		Kind:     worklet.ChangeComment,
		Mentions: []string{"ana"},
		Snapshot: worklet.Snapshot{
			Watchers:      []string{"ana", "bo"},
			Collaborators: []string{"ana"},
		},
	})
	if got := reasonOf(cands, "ana"); got != worklet.ReasonMention {
		t.Fatalf("ana hears under %q; a mention outranks a watch and a "+
			"collaboration", got)
	}
	if got := reasonOf(cands, "bo"); got != worklet.ReasonWatcher {
		t.Fatalf("bo hears under %q, want watcher", got)
	}
	seen := map[string]int{}
	for _, c := range cands {
		seen[c.Handle]++
	}
	for h, n := range seen {
		if n != 1 {
			t.Errorf("%s appears %d times; one handle hears once", h, n)
		}
	}
}

// A MENTION AND A NEW ASSIGNEE ARE ADDRESSED: the change asks each of them
// something, so the turn must answer.
func TestAMentionAndANewAssigneeAreAddressed(t *testing.T) {
	t.Parallel()
	n := &worklet.Notify{
		Kind:     worklet.ChangeAssignee,
		Mentions: []string{"ana"},
		Fields:   map[string]worklet.Delta{"assignee": {To: "bo"}},
		Snapshot: worklet.Snapshot{Assignee: "bo"},
	}
	reached := 0
	for _, c := range worklet.Candidates(n) {
		if c.Reason == worklet.ReasonMention || c.Reason == worklet.ReasonAssignee {
			reached++
			if !c.Addressed {
				t.Errorf("%s is not addressed under %q", c.Handle, c.Reason)
			}
		}
	}
	if reached != 2 {
		t.Fatalf("the mention and the assignee are %d candidate(s), want 2", reached)
	}
}

// THE FALLBACK IS A LIST: a lead filing an unassigned stray into their own
// team is dropped as the actor, and the project lead still gets the copy.
func TestALeadFilingAStrayStillReachesSomebody(t *testing.T) {
	t.Parallel()
	n := &worklet.Notify{
		Kind: worklet.ChangeCreated,
		Snapshot: worklet.Snapshot{
			RoutingUnitLead: "backend-lead",
			ProjectLead:     "vp-engineering",
		},
	}
	cands := worklet.Candidates(n)
	if len(cands) != 2 {
		t.Fatalf("the fallback was offered as %d candidate(s): %+v", len(cands), cands)
	}
	for _, c := range cands {
		if !c.FallbackOnly {
			t.Fatalf("%s is an ordinary candidate; both leads are fallbacks", c.Handle)
		}
	}
	routed := worklet.Route(cands, everyone, "backend-lead")
	if len(routed) != 1 || routed[0].Handle != "vp-engineering" {
		t.Fatalf("a lead filing into their own team routed to %v; the whole "+
			"point of the ordered list is that somebody still hears",
			handles(routed))
	}

	// When both survive, exactly one is woken, the lowest-ranked: a CHAIN,
	// not a distribution list.
	both := worklet.Route(cands, everyone, "somebody-else")
	if len(both) != 1 || both[0].Handle != "backend-lead" {
		t.Fatalf("with both leads present the change routed to %v; the fallback "+
			"is a chain and only its lowest rank is woken", handles(both))
	}
}

// A FALLBACK IS KEPT ONLY WHEN NOTHING ORDINARY SURVIVED.
func TestAFallbackYieldsToAnyOrdinaryCandidate(t *testing.T) {
	t.Parallel()
	cands := worklet.Candidates(&worklet.Notify{
		Kind: worklet.ChangeStatus,
		Snapshot: worklet.Snapshot{
			Assignee:        "ana",
			PrevStatusGroup: worklet.GroupActive,
			StatusGroup:     worklet.GroupDone,
			RoutingUnitLead: "backend-lead",
			ProjectLead:     "vp-engineering",
		},
	})
	routed := worklet.Route(cands, everyone, "bo")
	if got := handles(routed); len(got) != 1 || got[0] != "ana" {
		t.Fatalf("routed to %v; a fallback survives only when nothing ordinary "+
			"did", got)
	}

	// A watcher who is not woken still counts as an audience: the comment is
	// in its inbox, so the lead is not paged.
	quiet := worklet.Route(worklet.Candidates(&worklet.Notify{
		Kind: worklet.ChangeComment,
		Snapshot: worklet.Snapshot{
			Watchers:          []string{"cy"},
			RoutingUnitLead:   "backend-lead",
			CommentAuthorKind: worklet.AuthorAgent,
		},
	}), everyone, "bo")
	if len(quiet) != 0 {
		t.Fatalf("an agent's comment on a watched, unowned task woke %v; the "+
			"watcher is its audience and it asks nobody anything", handles(quiet))
	}
}

// ROUTED_TO IS ORDINARY: as a fallback, the new unit's lead would be dropped
// silently on any task with an assignee, a collaborator or a watcher.
func TestTheNewUnitsLeadHearsEvenOnAWatchedTask(t *testing.T) {
	t.Parallel()
	routed := worklet.Route(worklet.Candidates(&worklet.Notify{
		Kind: worklet.ChangeRouted,
		Snapshot: worklet.Snapshot{
			Assignee: "ana",
			Watchers: []string{"bo"},
			RoutedTo: "platform-lead",
		},
	}), everyone, "cy")
	if !slices.Contains(handles(routed), "platform-lead") {
		t.Fatalf("routed to %v and the new unit's lead is not among them",
			handles(routed))
	}
}

// THE ACTOR IS DROPPED, EXCEPT UNDER `unblocked` ([worklet.Reason.WakesActor]),
// on the record [worklet.Writer.TellUnblocked] actually publishes, taken off
// the log rather than built in a shape no writer produces.
func TestOnlyAnUnblockedNoticeReachesTheActor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []worklet.Relation{{
		Kind: worklet.RelationWaitingOn, Other: "t-1",
	}}
	// The harness writes as ana, whose dependent it is: the one record on
	// which the exception decides anything.
	dependent.Assignee = "ana"
	for _, task := range []worklet.Task{blocker, dependent} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+task.ID, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", task.ID, err)
		}
		r.drain()
	}
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-1", "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		&worklet.Notify{Kind: worklet.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()
	scan := scanUnblocked(t, r, 0)
	if len(scan.Pending) != 1 {
		t.Fatalf("the repair found %+v, want the one dependent", scan.Pending)
	}
	if _, err := r.writer.TellUnblocked(t.Context(), "op-tell", scan.Pending[0]); err != nil {
		t.Fatalf("TellUnblocked: %v", err)
	}

	// It concerns its assignee alone, under `unblocked`, UNADDRESSED.
	want := []routing.Candidate[worklet.Reason]{{Handle: "ana", Reason: worklet.ReasonUnblocked, Wakes: true}}
	cands := worklet.Candidates(r.lastWake())
	if !slices.Equal(cands, want) {
		t.Fatalf("the repair's record concerns %+v, want %+v", cands, want)
	}
	// AND IT REACHES HER THOUGH SHE WROTE IT...
	if got := worklet.Route(cands, everyone, "ana"); !slices.Equal(got, want) {
		t.Fatalf("ana's own unblocked record woke %+v, want %+v — a task of "+
			"hers becoming workable is news whoever writes it down", got, want)
	}
	// ...WHERE HER OWN CHANGE UNDER EVERY OTHER REASON REACHES NOBODY.
	own := worklet.Candidates(&worklet.Notify{
		Kind: worklet.ChangeStatus,
		Snapshot: worklet.Snapshot{
			Assignee: "ana", Reporter: "ana", Watchers: []string{"ana"},
			PrevStatusGroup: worklet.GroupActive, StatusGroup: worklet.GroupDone,
		},
	})
	if got := worklet.Route(own, everyone, "ana"); len(got) != 0 {
		t.Fatalf("ana's own close woke %v — she is told nothing she just did",
			handles(got))
	}
}

// AN UNWATCH IS A RECORD, NOT A WAKE: the candidate exists (the applier
// writes the row) and Route never returns it.
func TestAnUnwatchIsRecordedAndNotRouted(t *testing.T) {
	t.Parallel()
	cands := worklet.Candidates(&worklet.Notify{
		Kind:     worklet.ChangeWatchers,
		Snapshot: worklet.Snapshot{RemovedWatchers: []string{"ana"}},
	})
	if reasonOf(cands, "ana") != worklet.ReasonUnwatched {
		t.Fatalf("the removed watcher is not a candidate at all: %+v", cands)
	}
	if got := worklet.Route(cands, everyone, "lead"); len(got) != 0 {
		t.Fatalf("an inbox-only candidate was routed to %v", handles(got))
	}
}

// A HANDLE THE COMPANY NO LONGER EMPLOYS IS DROPPED AT ROUTE, NOT AT
// CANDIDATES: the row records everyone concerned, the wake reaches only
// those still there.
func TestADepartedHandleKeepsItsRowAndLosesItsWake(t *testing.T) {
	t.Parallel()
	cands := worklet.Candidates(&worklet.Notify{
		Kind:     worklet.ChangeComment,
		Mentions: []string{"ana", "gone"},
	})
	if reasonOf(cands, "gone") == "" {
		t.Fatal("a departed handle is missing from the record of who the change " +
			"concerned")
	}
	routed := worklet.Route(cands, func(h string) bool { return h != "gone" },
		"bo")
	if got := handles(routed); len(got) != 1 || got[0] != "ana" {
		t.Fatalf("routed to %v; the departed handle loses its wake and the "+
			"employed one keeps it", got)
	}
}

// A PERSON'S COMMENT ON YOUR TICKET IS THE ASK; ANOTHER AGENT'S IS NOT.
func TestOnlyAHumanCommentAddressesTheAssignee(t *testing.T) {
	t.Parallel()
	for kind, addressed := range map[worklet.AuthorKind]bool{
		worklet.AuthorHuman:    true,
		worklet.AuthorOperator: true,
		worklet.AuthorAgent:    false,
		worklet.AuthorSystem:   false,
	} {
		cands := worklet.Candidates(&worklet.Notify{
			Kind: worklet.ChangeComment,
			Snapshot: worklet.Snapshot{
				Assignee: "ana", CommentAuthorKind: kind,
			},
		})
		var got bool
		for _, c := range cands {
			if c.Handle == "ana" {
				got = c.Addressed
			}
		}
		if got != addressed {
			t.Errorf("a %s comment addresses the assignee = %v, want %v",
				kind, got, addressed)
		}
	}
}

// THE ANSWER REACHES WHOEVER ASKED, NOT WHOEVER ANSWERED: the asker is a
// snapshot field, not the comment's author.
func TestAnAnswerWakesTheAsker(t *testing.T) {
	t.Parallel()
	routed := worklet.Route(worklet.Candidates(&worklet.Notify{
		Kind:     worklet.ChangeCommentResolved,
		Snapshot: worklet.Snapshot{AnsweredAuthor: "founder"},
	}), everyone, "eng-1")
	if got := handles(routed); len(got) != 1 || got[0] != "founder" {
		t.Fatalf("an answer routed to %v rather than to whoever asked", got)
	}
}

// THE NINETEEN REASONS, AND THE EIGHT THAT OBLIGE AN ANSWER, which are not
// the nine of [worklet.DefaultPrimaryReasons] (what leads a PERSON's inbox):
// the two agree on four and differ on the rest, deliberately.
func TestTheReasonsAreAClosedSetAndTheTwoListsAreNotTheSame(t *testing.T) {
	t.Parallel()
	if len(worklet.Reasons) != 19 {
		t.Fatalf("%d reasons are enumerated; there are nineteen", len(worklet.Reasons))
	}
	addressed := 0
	for _, r := range worklet.Reasons {
		if !r.Valid() {
			t.Errorf("%q is enumerated and not valid", r)
		}
		if r.Addressed() {
			addressed++
		}
	}
	if addressed != 8 {
		t.Fatalf("%d reasons oblige an answer; the split is eight and eleven", addressed)
	}
	if worklet.Reason("overridden").Valid() {
		t.Error("a reason nothing writes is valid")
	}

	// Not the same list: without this, folding one into the other passes
	// every test in the tree.
	if len(worklet.DefaultPrimaryReasons) != 9 {
		t.Fatalf("%d reasons lead an inbox by default; there are nine",
			len(worklet.DefaultPrimaryReasons))
	}
	differ := 0
	for _, r := range worklet.DefaultPrimaryReasons {
		if !r.Valid() {
			t.Errorf("%q leads an inbox and is not a reason", r)
		}
		if !r.Addressed() {
			differ++
		}
	}
	if differ == 0 {
		t.Error("every reason that leads an inbox also obliges an answer, so " +
			"the two lists have been folded into one — they are different " +
			"questions: unblocked work leads an inbox and asks nothing, and " +
			"a thread you are in asks something and is not what an inbox opens with")
	}
}

// AN ASSIGNMENT TO THE HOLDER CONCERNS NOBODY: no change was made, so neither
// the holder nor the reporter is told one, while a real reassignment still is.
func TestAnAssignmentToTheHolderConcernsNobody(t *testing.T) {
	t.Parallel()
	same := &worklet.Notify{Kind: worklet.ChangeAssignee,
		Snapshot: worklet.Snapshot{Assignee: "ana", PrevAssignee: "ana", Reporter: "pm"}}
	if got := worklet.Candidates(same); got != nil {
		t.Errorf("an assignment to the holder concerns %+v, want nobody", got)
	}
	moved := &worklet.Notify{Kind: worklet.ChangeAssignee,
		Fields:   map[string]worklet.Delta{"assignee": {From: "ana", To: "bo"}},
		Snapshot: worklet.Snapshot{Assignee: "bo", PrevAssignee: "ana", Reporter: "pm"}}
	if heard, ok := worklet.CandidateFor(moved, "bo"); !ok || !heard.Wakes || !heard.Addressed {
		t.Errorf("the new holder of a real reassignment hears %+v, want woken and asked", heard)
	}
}
