package worklet_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY SNAPSHOT FIELD `Candidates` READS HAS A PRODUCER. An unfilled field
// makes its arm call `add("")`, which the candidate builder drops in silence,
// so the reason can never fire and nothing else would show it.
// `ParentAssignee` is the one field no [worklet.Wake] fills: the write's
// decide resolves it, so its producer is held by
// TestAnUpdatesWakeIsWhatItsWriteMoved and its gate by the internal
// TestTheParentIsNamedOnlyAcrossTheFinishedEdge.
func TestEveryRoutingFieldHasAProducer(t *testing.T) {
	t.Parallel()
	past := time.Date(2025, 3, 4, 9, 0, 0, 0, time.UTC)
	blocker := worklet.Task{
		ID: "b", Key: "ENG-1", Project: "ENG", Assignee: "bo",
		Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
	}
	done := worklet.Task{
		ID: "c", Key: "ENG-2", Project: "ENG", Assignee: "cy",
		Status: worklet.StatusDone, StatusGroup: worklet.GroupDone,
		Parent: ref("p"),
		Checklists: []worklet.Checklist{{ID: "l", Items: []worklet.ChecklistItem{
			{ID: "i1", Name: "one", Assignee: "di", Done: true},
		}}},
	}
	before := done
	before.Status, before.StatusGroup = worklet.StatusInProgress, worklet.GroupActive
	before.Checklists = []worklet.Checklist{{ID: "l", Items: []worklet.ChecklistItem{
		{ID: "i1", Name: "one", Assignee: "di"},
	}}}
	routed := blocker
	routed.RoutingUnit = "backend"

	for _, c := range []struct {
		field  string
		wake   worklet.Wake
		want   func(worklet.Snapshot) bool
		reason worklet.Reason
	}{
		{
			field: "Dependents", reason: worklet.ReasonBlocking,
			wake: worklet.Wake{
				Kind: worklet.ChangeRelations, Before: blocker, After: blocker,
				Dependents: []worklet.TaskParty{{Task: "c", Key: "ENG-2", Assignee: "cy"}},
			},
			want: func(s worklet.Snapshot) bool {
				return len(s.Dependents) == 1 && s.Dependents[0].Assignee == "cy"
			},
		},
		{
			field: "ChecklistAssignees", reason: worklet.ReasonChecklist,
			wake: worklet.Wake{
				Kind: worklet.ChangeChecklist, Before: before, After: done,
			},
			want: func(s worklet.Snapshot) bool {
				return slices.Contains(s.ChecklistAssignees, "di")
			},
		},
		{
			field: "CommentAsk", reason: worklet.ReasonAsked,
			wake: worklet.Wake{
				Kind: worklet.ChangeComment, Before: blocker, After: blocker,
				Comment: &worklet.Comment{ID: "m", Author: "ana", CreatedAt: past},
				Thread:  worklet.ThreadParties{Asked: "bo"},
			},
			want: func(s worklet.Snapshot) bool { return s.CommentAsk == "bo" },
		},
		{
			field: "AnsweredAuthor", reason: worklet.ReasonAnswered,
			wake: worklet.Wake{
				Kind: worklet.ChangeComment, Before: blocker, After: blocker,
				Comment: &worklet.Comment{ID: "m", Author: "bo", CreatedAt: past},
				Thread:  worklet.ThreadParties{AnsweredAuthor: "ana"},
			},
			want: func(s worklet.Snapshot) bool { return s.AnsweredAuthor == "ana" },
		},
		{
			field: "ThreadParticipants", reason: worklet.ReasonThread,
			wake: worklet.Wake{
				Kind: worklet.ChangeComment, Before: blocker, After: blocker,
				Comment: &worklet.Comment{ID: "m", Author: "bo", CreatedAt: past},
				Thread:  worklet.ThreadParties{Participants: []string{"ana", "cy"}},
			},
			want: func(s worklet.Snapshot) bool {
				return slices.Equal(s.ThreadParticipants, []string{"ana", "cy"})
			},
		},
		{
			field: "RoutedTo", reason: worklet.ReasonRoutedTo,
			wake: worklet.Wake{
				Kind: worklet.ChangeRouted, Before: blocker, After: routed,
			},
			want: func(s worklet.Snapshot) bool { return s.RoutedTo == "unit-lead" },
		},
	} {
		t.Run(c.field, func(t *testing.T) {
			t.Parallel()
			notify := c.wake.Notify(fixedLeads{unit: "unit-lead"})
			if notify == nil {
				t.Fatalf("%s: the wake is quiet, so this case tests nothing", c.field)
			}
			if !c.want(notify.Snapshot) {
				t.Errorf("Snapshot.%s was not filled: %+v — a routing arm reads "+
					"it, and an empty one is a reason that can never fire",
					c.field, notify.Snapshot)
			}
			// AND THE ROUTER REACHES SOMEBODY BY IT, UNDER THIS
			// REASON: a field never read, or whose handle an earlier
			// arm claims first, is the same silence.
			if !slices.ContainsFunc(worklet.Candidates(notify),
				func(got routing.Candidate[worklet.Reason]) bool { return got.Reason == c.reason }) {
				t.Errorf("Snapshot.%s is filled and no candidate came back "+
					"under %q: %+v", c.field, c.reason,
					worklet.Candidates(notify))
			}
		})
	}
}

// A ROUTED COMMIT THAT MOVES NO UNIT TELLS NO LEAD.
//
// The `reassign_unit` walk re-stamps every task in a unit; firing on the
// destination rather than the MOVE would wake one lead once per task.
func TestRoutedToFiresOnTheMoveAndNotTheDestination(t *testing.T) {
	t.Parallel()
	task := worklet.Task{
		ID: "t", Key: "ENG-1", Project: "ENG", RoutingUnit: "backend",
		Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted,
	}
	same := worklet.Wake{Kind: worklet.ChangeRouted, Before: task, After: task}.
		Notify(fixedLeads{unit: "unit-lead"})
	if same.Snapshot.RoutedTo != "" {
		t.Errorf("a routed commit that left the unit alone names %q as the new "+
			"lead — a walk re-stamping a unit would wake them once per task",
			same.Snapshot.RoutedTo)
	}
	// THE FALLBACK IS STILL CARRIED: the unit's lead hears when nobody else
	// is named, whether or not the commit moved anything.
	if same.Snapshot.RoutingUnitLead == "" {
		t.Error("a routed commit carries no unit lead at all, so the fallback " +
			"is gone as well as the ordinary candidate")
	}
}

// A CHECKLIST DIFF IS BY ITEM ID, never by position.
//
// A positional diff would wake every assignee on the checklist for a reorder.
func TestAChecklistReorderWakesNobody(t *testing.T) {
	t.Parallel()
	items := []worklet.ChecklistItem{
		{ID: "a", Name: "first", Assignee: "ana"},
		{ID: "b", Name: "second", Assignee: "bo"},
	}
	before := worklet.Task{ID: "t", Key: "ENG-1", Project: "ENG",
		Checklists: []worklet.Checklist{{ID: "l", Items: items}}}
	after := before
	after.Checklists = []worklet.Checklist{{ID: "l", Items: []worklet.ChecklistItem{
		items[1], items[0],
	}}}
	got := worklet.Wake{Kind: worklet.ChangeChecklist, Before: before, After: after}.
		Notify(fixedLeads{})
	if len(got.Snapshot.ChecklistAssignees) != 0 {
		t.Errorf("a reorder names %v as having had their item changed",
			got.Snapshot.ChecklistAssignees)
	}
	// AND A REASSIGNMENT NAMES BOTH ENDS: losing an item is news too.
	moved := before
	moved.Checklists = []worklet.Checklist{{ID: "l", Items: []worklet.ChecklistItem{
		{ID: "a", Name: "first", Assignee: "cy"}, items[1],
	}}}
	got = worklet.Wake{Kind: worklet.ChangeChecklist, Before: before, After: moved}.
		Notify(fixedLeads{})
	for _, want := range []string{"ana", "cy"} {
		if !slices.Contains(got.Snapshot.ChecklistAssignees, want) {
			t.Errorf("a reassigned item names %v and not %q",
				got.Snapshot.ChecklistAssignees, want)
		}
	}
	if slices.Contains(got.Snapshot.ChecklistAssignees, "bo") {
		t.Error("an untouched item's assignee is woken")
	}
}

// THE DERIVED COLLECTIONS ARE CUT RATHER THAN REFUSED, and deterministically.
//
// They come from ROWS, so a set over its cap is a task that grew, not a
// writer's mistake; refusing would fail a comment because a thread has many
// voices. Only what rides on the record is bounded: a participant past the cap
// is still a watcher.
func TestTheDerivedCollectionsAreCutNotRefused(t *testing.T) {
	t.Parallel()
	task := worklet.Task{ID: "t", Key: "ENG-1", Project: "ENG"}
	many := make([]string, 0, worklet.MaxThreadParticipants*3)
	for i := range cap(many) {
		many = append(many, "h"+itoa(i))
	}
	got := worklet.Wake{
		Kind: worklet.ChangeComment, Before: task, After: task,
		Comment: &worklet.Comment{ID: "m", Author: "ana"},
		Thread:  worklet.ThreadParties{Participants: many},
	}.Notify(fixedLeads{})
	if err := got.Validate(); err != nil {
		t.Fatalf("a long thread was refused rather than cut: %v", err)
	}
	if len(got.Snapshot.ThreadParticipants) != worklet.MaxThreadParticipants {
		t.Fatalf("the thread carries %d handles, want the cap of %d",
			len(got.Snapshot.ThreadParticipants), worklet.MaxThreadParticipants)
	}
	// THE FIRST N: who the thread is between, not who arrived last.
	if !slices.Equal(got.Snapshot.ThreadParticipants,
		many[:worklet.MaxThreadParticipants]) {
		t.Errorf("the cut kept %v rather than the first %d",
			got.Snapshot.ThreadParticipants, worklet.MaxThreadParticipants)
	}
}

// EVERY REASON HAS ITS OWN SENTENCE: no two reasons share an opener, and none
// falls through to the unknown-reason default.
func TestEveryReasonHasItsOwnOpener(t *testing.T) {
	t.Parallel()
	seen := map[string][]worklet.Reason{}
	for _, reason := range worklet.Reasons {
		text := worklet.Prompt{}.Build(
			promptNotification(reason, worklet.ChangeStatus), nil)
		opener, _, _ := strings.Cut(text, "\n")
		seen[opener] = append(seen[opener], reason)
	}
	for opener, reasons := range seen {
		if len(reasons) > 1 {
			t.Errorf("%v all open with %q — a reason whose sentence is "+
				"somebody else's is a reason nobody can act on", reasons, opener)
		}
	}
	if generic := seen["A task you are named on changed."]; len(generic) > 0 {
		t.Errorf("%v fall through to the unknown-reason default, which exists "+
			"for a kind a NEWER build routed", generic)
	}
}

func ref(s string) *string { return &s }

// NO REASON IS SHADOWED BY AN EARLIER ARM. `Candidates` dedupes on the
// HANDLE — a person hears under the first reason that names them — so an arm
// whose handle an earlier arm always names never fires: `blocking` adds only
// the blocker's assignee, whom the assignee arm names first, and `asked` and
// `answered` must still reach the assignee, the person most likely asked.
func TestNoReasonIsShadowedByAnEarlierArm(t *testing.T) {
	t.Parallel()
	// ONE HANDLE IN EVERY FIELD, so every arm competes for one person.
	const one = "ana"
	notify := &worklet.Notify{
		Kind:     worklet.ChangeComment,
		Mentions: []string{one},
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG",
			Assignee: one, Reporter: one, PrevAssignee: one,
			CommentAsk: one, AnsweredAuthor: one,
			ThreadParticipants: []string{one},
			Watchers:           []string{one},
			Collaborators:      []string{one},
			ProjectLead:        one,
		},
	}
	got := worklet.Candidates(notify)
	if len(got) == 0 {
		t.Fatal("one handle in every field routed to nobody")
	}
	// THE WINNER IS THE FIRST IN THE DECLARED PRECEDENCE: otherwise the
	// list every other surface reads disagrees with the function.
	want := worklet.Reasons[0]
	if got[0].Reason != want {
		t.Errorf("one handle in every field hears under %q and the precedence "+
			"list puts %q first — the list and Candidates disagree",
			got[0].Reason, want)
	}
	// AND EVERY REASON THAT CAN NAME A HANDLE NO OTHER ARM NAMES IS
	// REACHABLE, whatever the list says.
	for _, reason := range worklet.Reasons {
		alone := reasonAlone(reason)
		if alone == nil {
			continue
		}
		if !slices.ContainsFunc(worklet.Candidates(alone),
			func(c routing.Candidate[worklet.Reason]) bool { return c.Reason == reason }) {
			t.Errorf("%q cannot name anybody even on a notification built for "+
				"it alone — an earlier arm claims its handle", reason)
		}
	}
}

// THE DECLARED PRECEDENCE IS THE ORDER CANDIDATES OFFERS, ALL OF IT.
//
// [worklet.Reasons] states the precedence for the surfaces that print and
// filter by reason; Candidates' offer order IS the precedence, since
// internal/routing hears each handle under the first reason offered. The
// shadowing test above holds only the winner, so every kind is walked here with
// a DIFFERENT handle per routing field and the reasons offered must come back
// in the list's order. That covers every precedence: two reasons no kind
// offers together never compete for a handle.
func TestEveryKindOffersItsReasonsInTheDeclaredPrecedence(t *testing.T) {
	t.Parallel()
	place := make(map[worklet.Reason]int, len(worklet.Reasons))
	for i, reason := range worklet.Reasons {
		place[reason] = i
	}
	offered := map[worklet.Reason]bool{}
	for _, kind := range worklet.ChangeKinds {
		got := worklet.Candidates(everyRoutingField(kind))
		for i, c := range got {
			offered[c.Reason] = true
			if i > 0 && place[c.Reason] < place[got[i-1].Reason] {
				t.Errorf("a %s change offers %q after %q, and worklet.Reasons "+
					"puts %q first — the list and Candidates disagree about "+
					"who hears under what", kind, c.Reason, got[i-1].Reason,
					c.Reason)
			}
		}
	}
	// AND THE WALK OFFERED EVERY REASON, so none keeps its place in the
	// list only because no kind ever put it beside another.
	for _, reason := range worklet.Reasons {
		if !offered[reason] {
			t.Errorf("no kind offers %q with a handle of its own in every "+
				"field, so its place in the precedence is held by nothing", reason)
		}
	}
}

// everyRoutingField is a notification of kind naming a different handle in
// every field [worklet.Candidates] routes from, across the finished edge and
// handed from the previous assignee to the assignee (the two moves its deltas
// record), so each arm the kind opens offers a handle no other arm offers.
func everyRoutingField(kind worklet.ChangeKind) *worklet.Notify {
	return &worklet.Notify{
		Kind:     kind,
		Mentions: []string{"mentioned"},
		Fields: map[string]worklet.Delta{
			"assignee": {From: "previous-assignee", To: "assignee"},
			"status":   {From: "in_progress", To: "done"},
		},
		Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG",
			PrevStatusGroup:    worklet.GroupActive,
			StatusGroup:        worklet.GroupDone,
			Person:             "person",
			Assignee:           "assignee",
			Dependents:         []worklet.TaskParty{{Task: "d", Assignee: "dependent"}},
			CommentAsk:         "asked",
			AnsweredAuthor:     "answerer",
			PrevAssignee:       "previous-assignee",
			Reporter:           "reporter",
			ThreadParticipants: []string{"participant"},
			Unblocked:          []worklet.TaskParty{{Task: "u", Assignee: "unblocked"}},
			RoutedTo:           "new-unit-lead",
			ParentAssignee:     "parent-assignee",
			ChecklistAssignees: []string{"checklist-owner"},
			Collaborators:      []string{"collaborator"},
			Watchers:           []string{"watcher"},
			RemovedWatchers:    []string{"unwatched"},
			ProjectLead:        "project-lead",
			RoutingUnitLead:    "unit-lead",
		},
	}
}

// reasonAlone is a notification whose ONLY candidate should be this reason, or
// nil for the reasons that cannot be isolated.
//
// A DIFFERENT handle per field, so nothing shadows: the question is whether an
// arm can fire at all, not which wins.
func reasonAlone(reason worklet.Reason) *worklet.Notify {
	base := func(kind worklet.ChangeKind) *worklet.Notify {
		return &worklet.Notify{Kind: kind, Snapshot: worklet.Snapshot{
			Key: "ENG-1", Project: "ENG",
		}}
	}
	switch reason {
	case worklet.ReasonMention:
		n := base(worklet.ChangeComment)
		n.Mentions = []string{"m"}
		return n
	case worklet.ReasonPrioritised:
		n := base(worklet.ChangePrioritised)
		n.Snapshot.Person = "p"
		return n
	case worklet.ReasonBlocking:
		n := base(worklet.ChangeRelations)
		n.Snapshot.Assignee = "b"
		n.Snapshot.Dependents = []worklet.TaskParty{{Task: "d", Assignee: "x"}}
		return n
	case worklet.ReasonAsked:
		n := base(worklet.ChangeComment)
		n.Snapshot.CommentAsk = "k"
		return n
	case worklet.ReasonAnswered:
		n := base(worklet.ChangeComment)
		n.Snapshot.AnsweredAuthor = "w"
		return n
	case worklet.ReasonAssignee:
		n := base(worklet.ChangeStatus)
		n.Snapshot.Assignee = "a"
		return n
	case worklet.ReasonUnassigned:
		// HANDED TO NOBODY: the assignee delta is what tells its holder,
		// whatever kind the record ends with.
		n := base(worklet.ChangeAssignee)
		n.Fields = map[string]worklet.Delta{"assignee": {From: "u"}}
		n.Snapshot.PrevAssignee = "u"
		return n
	case worklet.ReasonReporter:
		n := base(worklet.ChangeComment)
		n.Snapshot.Reporter = "r"
		return n
	case worklet.ReasonThread:
		n := base(worklet.ChangeComment)
		n.Snapshot.ThreadParticipants = []string{"t"}
		return n
	case worklet.ReasonUnblocked:
		// THE REPAIR'S OWN SHAPE ([worklet.Writer.TellUnblocked], the one
		// writer of the field): a late relations commit naming the
		// dependent and its assignee alone.
		n := base(worklet.ChangeRelations)
		n.Late = true
		n.Snapshot.Unblocked = []worklet.TaskParty{{Task: "o", Assignee: "n"}}
		return n
	case worklet.ReasonRoutedTo:
		n := base(worklet.ChangeRouted)
		n.Snapshot.RoutedTo = "ro"
		return n
	case worklet.ReasonParentAssignee:
		n := base(worklet.ChangeStatus)
		n.Snapshot.PrevStatusGroup = worklet.GroupActive
		n.Snapshot.StatusGroup = worklet.GroupDone
		n.Snapshot.ParentAssignee = "pa"
		return n
	case worklet.ReasonChecklist:
		n := base(worklet.ChangeChecklist)
		n.Snapshot.ChecklistAssignees = []string{"c"}
		return n
	case worklet.ReasonCollaborator:
		n := base(worklet.ChangeStatus)
		n.Snapshot.Collaborators = []string{"co"}
		return n
	case worklet.ReasonWatcher:
		n := base(worklet.ChangeStatus)
		n.Snapshot.Watchers = []string{"wa"}
		return n
	case worklet.ReasonUnwatched:
		n := base(worklet.ChangeWatchers)
		n.Snapshot.RemovedWatchers = []string{"uw"}
		return n
	case worklet.ReasonPurged:
		n := base(worklet.ChangePurged)
		n.Snapshot.ProjectLead = "pl"
		return n
	case worklet.ReasonLeadFallback:
		n := base(worklet.ChangeCreated)
		n.Snapshot.ProjectLead = "lf"
		return n
	}
	return nil
}
