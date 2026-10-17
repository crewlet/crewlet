package worklet_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY SNAPSHOT FIELD `Candidates` READS HAS A PRODUCER, and this is the
// guard the tree did not have.
//
// Six of them did not: `ParentAssignee`, `Dependents`, `ChecklistAssignees`,
// `CommentAsk`, `AnsweredAuthor`, `ThreadParticipants` and `RoutedTo` were
// declared, documented, read by a routing arm and written by NOTHING. Every
// one of those arms called `add("")`, which the candidate builder's own guard
// drops in silence — so seven reasons could never fire and no test, no log
// line and no screen could tell that apart from a company where nothing
// happened.
//
// The table is that missing link. A field a routing arm reads and no producer
// fills makes this go red, which is the only way the omission is visible.
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
			field: "ParentAssignee", reason: worklet.ReasonParentAssignee,
			wake: worklet.Wake{
				Kind: worklet.ChangeStatus, Before: before, After: done,
				Parent: &worklet.TaskParty{Task: "p", Key: "ENG-9", Assignee: "pa"},
			},
			want: func(s worklet.Snapshot) bool { return s.ParentAssignee == "pa" },
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
			// REASON. A field filled but never read is the same
			// silence under a different name — and so is a handle an
			// EARLIER arm claimed first, which is exactly what hid
			// `blocking`: the only handle it ever adds is the
			// blocker's assignee, and the generic assignee arm ran
			// before it, so the reason looked implemented from every
			// angle except the one that counts.
			if !slices.ContainsFunc(worklet.Candidates(notify),
				func(got worklet.Candidate) bool { return got.Reason == c.reason }) {
				t.Errorf("Snapshot.%s is filled and no candidate came back "+
					"under %q: %+v", c.field, c.reason,
					worklet.Candidates(notify))
			}
		})
	}
}

// A ROUTED COMMIT THAT MOVES NO UNIT TELLS NO LEAD.
//
// The `reassign_unit` walk re-stamps every task in a unit, and a `routed_to`
// that fired on the destination rather than on the MOVE would wake one lead
// once per task — a thousand times for a bookkeeping correction.
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
	// THE FALLBACK IS STILL CARRIED, which is the distinction: the unit's
	// lead is who hears when nobody else is named, whether or not this
	// commit moved anything.
	if same.Snapshot.RoutingUnitLead == "" {
		t.Error("a routed commit carries no unit lead at all, so the fallback " +
			"is gone as well as the ordinary candidate")
	}
}

// A CHECKLIST DIFF IS BY ITEM ID, never by position.
//
// A positional diff reports the whole list as changed the first time somebody
// drags a line, which would wake every assignee on the checklist for a
// reorder nobody needs to know about.
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
	// AND A REASSIGNMENT NAMES BOTH ENDS, because losing an item is news
	// to the person who had it as much as gaining one is to the person
	// who has it now.
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

// THE PARENT HEARS ONLY ACROSS THE FINISHED EDGE, and the gate is the same
// predicate the router applies — computed from the same two values, so a field
// filled here can never be one the router will not read.
func TestTheParentHearsOnlyAcrossTheFinishedEdge(t *testing.T) {
	t.Parallel()
	parent := &worklet.TaskParty{Task: "p", Key: "ENG-9", Assignee: "pa"}
	todo := worklet.Task{ID: "t", Key: "ENG-1", Project: "ENG", Parent: ref("p"),
		Status: worklet.StatusTodo, StatusGroup: worklet.GroupNotStarted}
	doing := todo
	doing.Status, doing.StatusGroup = worklet.StatusInProgress, worklet.GroupActive
	finished := todo
	finished.Status, finished.StatusGroup = worklet.StatusDone, worklet.GroupDone

	for _, c := range []struct {
		name          string
		before, after worklet.Task
		kind          worklet.ChangeKind
		want          bool
	}{
		{"into_done", doing, finished, worklet.ChangeStatus, true},
		{"out_of_done", finished, doing, worklet.ChangeStatus, true},
		{"within_open", todo, doing, worklet.ChangeStatus, false},
		{"not_a_status_change", doing, doing, worklet.ChangeFields, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := worklet.Wake{
				Kind: c.kind, Before: c.before, After: c.after, Parent: parent,
			}.Notify(fixedLeads{})
			named := got.Snapshot.ParentAssignee != ""
			if named != c.want {
				t.Errorf("ParentAssignee named=%v, want %v — the gate has to be "+
					"the router's own, or a field is filled for a record whose "+
					"arm will not fire", named, c.want)
			}
		})
	}
}

// THE DERIVED COLLECTIONS ARE CUT RATHER THAN REFUSED, and deterministically.
//
// They come from ROWS, so a set over its cap is a task that grew rather than a
// writer that asked for too much — and refusing the write would fail somebody's
// comment because a thread has many voices. What is bounded is what rides on
// the record; nobody is silenced, because a participant past the cap is still
// a watcher.
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
	// THE FIRST N, which is who the thread is between rather than who
	// happened to arrive last.
	if !slices.Equal(got.Snapshot.ThreadParticipants,
		many[:worklet.MaxThreadParticipants]) {
		t.Errorf("the cut kept %v rather than the first %d",
			got.Snapshot.ThreadParticipants, worklet.MaxThreadParticipants)
	}
}

// EVERY REASON HAS ITS OWN SENTENCE, and this is what the three-function
// dispatch could not promise.
//
// The opener was chosen by [Reason.Primary] across three functions, and six
// arms sat in the half their own reason could never reach: a blocker's
// assignee read "a task you are named on changed", and somebody whose work had
// just become startable read "a task you are watching changed". Each arm
// looked right beside the others in its own function.
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

// NO REASON IS SHADOWED BY AN EARLIER ARM, which is the failure mode the
// six dead routing fields turned out to share with three live ones.
//
// `Candidates` dedupes on the HANDLE — the first reason that names somebody is
// the one they hear under — so an arm whose handle an earlier arm also names
// is an arm that never fires for that person. `blocking` was the pure case:
// the only handle it ever adds is the blocker's own assignee, and the generic
// assignee arm ran first, so the reason could not name anybody on any commit.
// `asked` and `answered` were the subtle case — they worked for a colleague
// and fell silent for the assignee, which is the person most likely to be
// asked.
//
// Nothing in the build could see any of it. The field was set, the arm was
// written, the constant was in the precedence list and the parity test agreed
// with the applier — about the wrong answer.
func TestNoReasonIsShadowedByAnEarlierArm(t *testing.T) {
	t.Parallel()
	// ONE HANDLE IN EVERY FIELD. If two arms can name one person, this is
	// the notification that proves which of them wins — and every reason
	// that loses here is one that cannot fire when it matters most.
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
	// THE WINNER IS THE FIRST IN THE DECLARED PRECEDENCE, which is what
	// makes the list a rule rather than a description: a reason that wins
	// out of order means the list and the function disagree, and the list
	// is what every other surface reads.
	want := worklet.Reasons[0]
	if got[0].Reason != want {
		t.Errorf("one handle in every field hears under %q and the precedence "+
			"list puts %q first — the list and Candidates disagree",
			got[0].Reason, want)
	}
	// AND EVERY REASON THAT CAN NAME A HANDLE NO OTHER ARM NAMES IS
	// REACHABLE, which is the other half: a reason whose every possible
	// handle is claimed earlier is unreachable whatever the list says.
	for _, reason := range worklet.Reasons {
		alone := reasonAlone(reason)
		if alone == nil {
			continue
		}
		if !slices.ContainsFunc(worklet.Candidates(alone),
			func(c worklet.Candidate) bool { return c.Reason == reason }) {
			t.Errorf("%q cannot name anybody even on a notification built for "+
				"it alone — an earlier arm claims its handle", reason)
		}
	}
}

// reasonAlone is a notification whose ONLY candidate should be this reason, or
// nil for the reasons that cannot be isolated.
//
// Built with a DIFFERENT handle per field, so nothing shadows anything: the
// question here is whether an arm can fire at all, not which arm wins.
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
		n := base(worklet.ChangeAssignee)
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
		n := base(worklet.ChangeStatus)
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
