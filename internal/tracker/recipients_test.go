package tracker_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// everyone is a registry that employs anybody.
func everyone(string) bool { return true }

// reasonOf finds the reason a handle appears under, or "".
func reasonOf(cands []tracker.Candidate, handle string) tracker.Reason {
	for _, c := range cands {
		if c.Handle == handle {
			return c.Reason
		}
	}
	return ""
}

func handles(cands []tracker.Candidate) []string {
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
	if got := tracker.Candidates(nil, false); got != nil {
		t.Fatalf("a nil notification produced %d candidate(s)", len(got))
	}
}

// THE FIRST REASON PER HANDLE WINS.
//
// A person who is mentioned AND watching is told they were mentioned, which is
// the stronger fact and the one they will act on.
func TestTheFirstReasonPerHandleWins(t *testing.T) {
	t.Parallel()
	cands := tracker.Candidates(&tracker.Notify{
		Kind:     tracker.ChangeComment,
		Mentions: []string{"ana"},
		Snapshot: tracker.Snapshot{
			Watchers:      []string{"ana", "bo"},
			Collaborators: []string{"ana"},
		},
	}, false)
	if got := reasonOf(cands, "ana"); got != tracker.ReasonMention {
		t.Fatalf("ana hears under %q; a mention outranks a watch and a "+
			"collaboration", got)
	}
	if got := reasonOf(cands, "bo"); got != tracker.ReasonWatcher {
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

// A BATCH COPY IS NEVER ADDRESSED.
//
// A bulk sprint plan is thirty facts to absorb, not thirty asks — and an
// addressed wake is one a turn is required to answer.
func TestABatchCopyIsNeverAddressed(t *testing.T) {
	t.Parallel()
	n := &tracker.Notify{
		Kind:     tracker.ChangeAssignee,
		Mentions: []string{"ana"},
		Snapshot: tracker.Snapshot{Assignee: "bo"},
	}
	for _, c := range tracker.Candidates(n, false) {
		if c.Reason == tracker.ReasonMention || c.Reason == tracker.ReasonAssignee {
			if !c.Addressed {
				t.Fatalf("%s is not addressed under %q on a single write",
					c.Handle, c.Reason)
			}
		}
	}
	for _, c := range tracker.Candidates(n, true) {
		if c.Addressed {
			t.Errorf("%s is addressed under %q inside a batch", c.Handle, c.Reason)
		}
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
	n := &tracker.Notify{
		Kind: tracker.ChangeCreated,
		Snapshot: tracker.Snapshot{
			RoutingUnitLead: "backend-lead",
			ProjectLead:     "vp-engineering",
		},
	}
	cands := tracker.Candidates(n, false)
	if len(cands) != 2 {
		t.Fatalf("the fallback was offered as %d candidate(s): %+v", len(cands), cands)
	}
	for _, c := range cands {
		if !c.FallbackOnly {
			t.Fatalf("%s is an ordinary candidate; both leads are fallbacks", c.Handle)
		}
	}
	routed := tracker.Route(cands, everyone, "backend-lead")
	if len(routed) != 1 || routed[0].Handle != "vp-engineering" {
		t.Fatalf("a lead filing into their own team routed to %v; the whole "+
			"point of the ordered list is that somebody still hears",
			handles(routed))
	}

	// AND WHEN BOTH SURVIVE, EXACTLY ONE IS WOKEN — the lowest-ranked. The
	// list is a fallback CHAIN, not a distribution list: waking both the
	// unit lead and the VP for every unassigned task is how a fallback
	// becomes something people filter out.
	both := tracker.Route(cands, everyone, "somebody-else")
	if len(both) != 1 || both[0].Handle != "backend-lead" {
		t.Fatalf("with both leads present the change routed to %v; the fallback "+
			"is a chain and only its lowest rank is woken", handles(both))
	}
}

// A FALLBACK IS KEPT ONLY WHEN NOTHING ORDINARY SURVIVED.
func TestAFallbackYieldsToAnyOrdinaryCandidate(t *testing.T) {
	t.Parallel()
	cands := tracker.Candidates(&tracker.Notify{
		Kind: tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{
			Assignee:        "ana",
			RoutingUnitLead: "backend-lead",
			ProjectLead:     "vp-engineering",
		},
	}, false)
	routed := tracker.Route(cands, everyone, "bo")
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
	routed := tracker.Route(tracker.Candidates(&tracker.Notify{
		Kind: tracker.ChangeRouted,
		Snapshot: tracker.Snapshot{
			Assignee: "ana",
			Watchers: []string{"bo"},
			RoutedTo: "platform-lead",
		},
	}, false), everyone, "cy")
	if !slices.Contains(handles(routed), "platform-lead") {
		t.Fatalf("routed to %v and the new unit's lead is not among them",
			handles(routed))
	}
}

// THE ACTOR IS DROPPED, WITH EXACTLY ONE EXCEPTION.
//
// Telling somebody what they just did is a round a model spends on nothing.
// An unblocked notice is the exception because it is about somebody ELSE's
// task becoming workable, so the person who closed the blocker is exactly who
// needs to hear it.
func TestOnlyAnUnblockedNoticeReachesTheActor(t *testing.T) {
	t.Parallel()
	cands := tracker.Candidates(&tracker.Notify{
		Kind: tracker.ChangeStatus,
		Snapshot: tracker.Snapshot{
			Assignee:        "ana",
			PrevStatusGroup: tracker.GroupActive,
			StatusGroup:     tracker.GroupDone,
			Unblocked:       []tracker.TaskParty{{Task: "t-2", Assignee: "ana"}},
		},
	}, false)
	// ANA APPEARS TWICE, and that is the point: one commit legitimately
	// tells her "your task is done" and "this other one is now workable".
	// Deduping the second away under the first is how the person who
	// closed the blocker never learns what they unblocked.
	if len(cands) != 2 {
		t.Fatalf("ana is a candidate %d time(s); the unblocked notice is about "+
			"another task and is not deduped against reasons about this one: %+v",
			len(cands), cands)
	}
	routed := tracker.Route(cands, everyone, "ana")
	if len(routed) != 1 {
		t.Fatalf("routed to %v, want ana alone under unblocked", handles(routed))
	}
	if routed[0].Handle != "ana" || routed[0].Reason != tracker.ReasonUnblocked {
		t.Fatalf("routed to %s under %q", routed[0].Handle, routed[0].Reason)
	}
	// And an unblocked notice is never ADDRESSED: a turn that cannot start
	// yet would otherwise be forced to post something on every unblock.
	if routed[0].Addressed {
		t.Error("an unblocked notice is addressed")
	}
	if routed[0].Task != "t-2" {
		t.Errorf("the unblocked notice names task %q; it is about the task that "+
			"became workable, not the one that closed", routed[0].Task)
	}
}

// AN UNWATCH IS A RECORD, NOT A WAKE.
//
// A human learns that a lead removed her watch; an agent is not woken about a
// decision that was not its own. So the candidate exists — the applier writes
// the row — and Route never returns it.
func TestAnUnwatchIsRecordedAndNotRouted(t *testing.T) {
	t.Parallel()
	cands := tracker.Candidates(&tracker.Notify{
		Kind:     tracker.ChangeWatchers,
		Snapshot: tracker.Snapshot{RemovedWatchers: []string{"ana"}},
	}, false)
	if reasonOf(cands, "ana") != tracker.ReasonUnwatched {
		t.Fatalf("the removed watcher is not a candidate at all: %+v", cands)
	}
	if got := tracker.Route(cands, everyone, "lead"); len(got) != 0 {
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
	cands := tracker.Candidates(&tracker.Notify{
		Kind:     tracker.ChangeComment,
		Snapshot: tracker.Snapshot{Watchers: []string{"ana", "gone"}},
	}, false)
	if reasonOf(cands, "gone") == "" {
		t.Fatal("a departed handle is missing from the record of who the change " +
			"concerned")
	}
	routed := tracker.Route(cands, func(h string) bool { return h != "gone" }, "bo")
	if slices.Contains(handles(routed), "gone") {
		t.Fatalf("a departed handle was woken: %v", handles(routed))
	}
}

// A PERSON'S COMMENT ON YOUR TICKET IS THE ASK; ANOTHER AGENT'S IS NOT.
func TestOnlyAHumanCommentAddressesTheAssignee(t *testing.T) {
	t.Parallel()
	for kind, addressed := range map[tracker.AuthorKind]bool{
		tracker.AuthorHuman:    true,
		tracker.AuthorOperator: true,
		tracker.AuthorAgent:    false,
		tracker.AuthorSystem:   false,
	} {
		cands := tracker.Candidates(&tracker.Notify{
			Kind: tracker.ChangeComment,
			Snapshot: tracker.Snapshot{
				Assignee: "ana", CommentAuthorKind: kind,
			},
		}, false)
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
	routed := tracker.Route(tracker.Candidates(&tracker.Notify{
		Kind:     tracker.ChangeCommentResolved,
		Snapshot: tracker.Snapshot{AnsweredAuthor: "founder"},
	}, false), everyone, "eng-1")
	if got := handles(routed); len(got) != 1 || got[0] != "founder" {
		t.Fatalf("an answer routed to %v rather than to whoever asked", got)
	}
}

// THE TWENTY REASONS SPLIT EIGHT AND TWELVE.
func TestTheReasonsAreAClosedSetSplitEightAndTwelve(t *testing.T) {
	t.Parallel()
	if len(tracker.Reasons) != 20 {
		t.Fatalf("%d reasons are enumerated; there are twenty", len(tracker.Reasons))
	}
	primary := 0
	for _, r := range tracker.Reasons {
		if !r.Valid() {
			t.Errorf("%q is enumerated and not valid", r)
		}
		if r.Primary() {
			primary++
		}
	}
	if primary != 8 {
		t.Fatalf("%d reasons are primary; the split is eight and twelve", primary)
	}
	if tracker.Reason("overridden").Valid() {
		t.Error("a reason nothing writes is valid")
	}
}
