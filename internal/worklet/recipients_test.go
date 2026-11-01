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

// A QUIET COMMIT CONCERNS NOBODY, ON BOTH SURFACES.
//
// The translator acks a quiet commit so the feed never routes one, and the
// applier calls this same function — so a quiet walk must leave no
// notification row either. Without that one rule the two surfaces disagree and
// an inbox fills with changes the company deliberately did not announce.
func TestAQuietCommitConcernsNobody(t *testing.T) {
	t.Parallel()
	if got := worklet.Candidates(nil); got != nil {
		t.Fatalf("a nil notification produced %d candidate(s)", len(got))
	}
}

// THE FIRST REASON PER HANDLE WINS.
//
// A person who is mentioned AND watching is told they were mentioned, which is
// the stronger fact and the one they will act on.
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

// A MENTION AND A NEW ASSIGNEE ARE ADDRESSED: each is somebody the change
// asks something of, and an addressed wake is one a turn is required to
// answer.
func TestAMentionAndANewAssigneeAreAddressed(t *testing.T) {
	t.Parallel()
	n := &worklet.Notify{
		Kind:     worklet.ChangeAssignee,
		Mentions: []string{"ana"},
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

// THE FALLBACK IS A LIST, AND THE CASE THAT MAKES IT ONE.
//
// A lead files an unassigned stray into their own team. With a single
// fallback candidate, Route's actor drop removes it and the task reaches
// NOBODY. With the ordered list, the unit lead is dropped as the actor and the
// project lead gets the copy.
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

	// AND WHEN BOTH SURVIVE, EXACTLY ONE IS WOKEN — the lowest-ranked. The
	// list is a fallback CHAIN, not a distribution list: waking both the
	// unit lead and the VP for every unassigned task is how a fallback
	// becomes something people filter out.
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
			RoutingUnitLead: "backend-lead",
			ProjectLead:     "vp-engineering",
		},
	})
	routed := worklet.Route(cands, everyone, "bo")
	if got := handles(routed); len(got) != 1 || got[0] != "ana" {
		t.Fatalf("routed to %v; a fallback survives only when nothing ordinary "+
			"did", got)
	}
}

// ROUTED_TO IS ORDINARY, AND THAT IS THE WHOLE REASON IT EXISTS.
//
// As a fallback it would be kept only when no ordinary candidate survived — so
// on any task that still has an assignee, a collaborator or a watcher, the new
// unit's lead would have been dropped in silence.
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

// THE ACTOR IS DROPPED, WITH EXACTLY ONE EXCEPTION — on the record the
// unblocked repair actually writes.
//
// Telling somebody what they just did is a round a model spends on nothing,
// so a person's own change reaches nobody under any other reason. `unblocked`
// is the exception: that a task of yours became workable is news whoever
// wrote the record saying so. That record is [worklet.Writer.TellUnblocked]'s
// — a relations commit on the DEPENDENT naming its assignee and nobody else —
// and this case takes it off the log as the repair publishes it, rather than
// building a notification shape no writer produces.
func TestOnlyAnUnblockedNoticeReachesTheActor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker, dependent := newTask("t-1"), newTask("t-2")
	dependent.Relations = []worklet.Relation{{
		Kind: worklet.RelationWaitingOn, Other: "t-1",
	}}
	// THE HARNESS WRITES AS ANA, and the dependent is hers: the one record on
	// which the exception decides anything. The engine's own repair writes
	// as its node, so its notice never meets its author.
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

	// THE REPAIR'S RECORD CONCERNS ITS ASSIGNEE ALONE, under `unblocked` and
	// UNADDRESSED: a turn that cannot start yet would otherwise be forced to
	// post something on every unblock.
	want := []routing.Candidate[worklet.Reason]{{Handle: "ana", Reason: worklet.ReasonUnblocked}}
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

// AN UNWATCH IS A RECORD, NOT A WAKE.
//
// A human learns that a lead removed her watch; an agent is not woken about a
// decision that was not its own. So the candidate exists — the applier writes
// the row — and Route never returns it.
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
// CANDIDATES.
//
// The row is a complete record of everyone the change concerned; the wake goes
// only to the people still there. Dropping a departed handle earlier would
// lose the record, and later would publish into a mailbox nothing consumes.
func TestADepartedHandleKeepsItsRowAndLosesItsWake(t *testing.T) {
	t.Parallel()
	cands := worklet.Candidates(&worklet.Notify{
		Kind:     worklet.ChangeComment,
		Snapshot: worklet.Snapshot{Watchers: []string{"ana", "gone"}},
	})
	if reasonOf(cands, "gone") == "" {
		t.Fatal("a departed handle is missing from the record of who the change " +
			"concerned")
	}
	routed := worklet.Route(cands, func(h string) bool { return h != "gone" },
		"bo")
	if slices.Contains(handles(routed), "gone") {
		t.Fatalf("a departed handle was woken: %v", handles(routed))
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

// THE ANSWER REACHES WHOEVER ASKED, NOT WHOEVER ANSWERED.
//
// Routing off the comment's author would wake the seat that just answered and
// leave the person who asked unwoken, which is why the asker is a snapshot
// field rather than a lookup.
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

// THE EIGHTEEN REASONS, AND THE EIGHT THAT OBLIGE AN ANSWER.
//
// AND THE OTHER EIGHT ARE A DIFFERENT EIGHT. [worklet.DefaultPrimaryReasons]
// is which reasons lead a PERSON's inbox, and it is not this list — the two
// agree on four and differ on four, deliberately. They were both called
// "primary" and both eight long, which is how a reader comes to use one where
// the other belongs, and nothing failed when they did.
func TestTheReasonsAreAClosedSetAndTheTwoEightsAreNotTheSame(t *testing.T) {
	t.Parallel()
	if len(worklet.Reasons) != 18 {
		t.Fatalf("%d reasons are enumerated; there are eighteen", len(worklet.Reasons))
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
		t.Fatalf("%d reasons oblige an answer; the split is eight and ten", addressed)
	}
	if worklet.Reason("overridden").Valid() {
		t.Error("a reason nothing writes is valid")
	}

	// THE TWO LISTS ARE THE SAME LENGTH AND NOT THE SAME LIST. Without
	// this, folding one into the other passes every test in the tree.
	if len(worklet.DefaultPrimaryReasons) != 8 {
		t.Fatalf("%d reasons lead an inbox by default; there are eight",
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
