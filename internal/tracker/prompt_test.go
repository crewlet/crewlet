package tracker_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY ROUTING REASON PRODUCES A PROMPT, AND EVERY PRIMARY ONE ASKS.
//
// # Why this is a walk over the enum rather than a list of cases
//
// The router distinguishes twenty reasons and the prompt frames three, so
// eighteen of them reach a default. That is deliberate — what a RECIPIENT
// needs to be told collapses to "somebody is asking you", "this is your work"
// and "this is activity you follow" — but it means a reason added later
// silently takes the weakest frame, and the seat is told it is merely watching
// something it was in fact assigned. Walking the enum is what makes that
// visible: a new reason has to be classified here or the test names it.
func TestEveryReasonIsFramedAndOnlyThePrimaryOnesAsk(t *testing.T) {
	t.Parallel()
	for _, reason := range tracker.Reasons {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			n := promptNotification(reason, tracker.ChangeStatus)
			body := tracker.Prompt{}.Build(n, nil)
			if strings.TrimSpace(body) == "" {
				t.Fatal("this reason renders an empty prompt, so the seat is " +
					"woken with nothing to act on")
			}
			if !strings.Contains(body, "**Task:**") {
				t.Error("the prompt names no task, and every rule in it rests " +
					"on the seat knowing which one")
			}
			if got := (tracker.Prompt{}).Addressed(n); got != reason.Primary() {
				t.Errorf("Addressed = %v and this reason is primary=%v — an "+
					"addressed turn may not end in silence, and marking a "+
					"watcher addressed makes a seat answer every field change "+
					"in its unit's projects", got, reason.Primary())
			}
		})
	}
}

// A FALLBACK EXPLAINS ITSELF AND ASKS FOR A DECISION.
//
// A directed routing carries its own signal — being assigned or named says
// what is wanted — while a fallback says only that nobody else here was
// available, which is a fact about the org chart rather than about the work.
// Left unexplained, a lead reads it as "this is mine" and quietly absorbs
// every unowned task in their project.
func TestAFallbackRoutingExplainsItself(t *testing.T) {
	t.Parallel()
	body := tracker.Prompt{}.Build(
		promptNotification(tracker.ReasonLeadFallback, tracker.ChangeCreated), nil)
	for _, want := range []string{
		"Why you received this", "FALLBACK", "Delegate", "Escalate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the fallback prompt does not say %q:\n%s", want, body)
		}
	}
	// AND NOT TWO SETS OF STEPS. The delegate / take it / escalate block
	// IS this seat's instruction, and a second list under a second
	// heading makes a lead read both and follow neither.
	if strings.Contains(body, "How to handle this") {
		t.Error("a fallback gets both the decision block and the general " +
			"handling list, which is two sets of steps for one wake")
	}
}

// A REMOVED TASK SENDS NOBODY TO READ IT.
//
// Fetching a task that no longer exists costs the seat a round and a failed
// tool call, and the answer it gets back says nothing about what to do.
func TestARemovedTaskIsNotSentForRecon(t *testing.T) {
	t.Parallel()
	n := promptNotification(tracker.ReasonAssignee, tracker.ChangeRemoved)
	body := tracker.Prompt{}.Build(n, nil)
	if strings.Contains(body, "Get full context") {
		t.Errorf("a removed task sends the seat to read it:\n%s", body)
	}
	if !strings.Contains(body, "no longer exists") {
		t.Errorf("the prompt does not say the task is gone:\n%s", body)
	}
}

// A REPAIR SAYS IT IS ONE.
//
// A late notice reaches somebody hours after the change, and a reader who
// cannot tell that from a fresh event re-reads a thread looking for what just
// moved.
func TestALateNoticeSaysWhyItIsLate(t *testing.T) {
	t.Parallel()
	n := promptNotification(tracker.ReasonUnblocked, tracker.ChangeRelations)
	n.Metadata[tracker.MetaLate] = "true"
	body := tracker.Prompt{}.Build(n, nil)
	if !strings.Contains(body, "repair") {
		t.Errorf("a late notice reads as a fresh change:\n%s", body)
	}
}

// THE ONE REASON THAT WAKES ITS OWN ACTOR IS THE ONE THAT IS A CONSEQUENCE.
//
// A person who closed a blocker learns from this that the task it was blocking
// is now workable — which they could not have derived from their own write.
// Every other change here is something the actor already knows, and waking
// them about it is a seat reading its own comment back to itself.
func TestOnlyAnUnblockedNoticeWakesItsActor(t *testing.T) {
	t.Parallel()
	for _, reason := range tracker.Reasons {
		want := reason == tracker.ReasonUnblocked
		if got := (tracker.Prompt{}).WakesActor(string(reason)); got != want {
			t.Errorf("WakesActor(%s) = %v, want %v", reason, got, want)
		}
	}
}

func promptNotification(reason tracker.Reason, kind tracker.ChangeKind) notify.Inbound {
	return notify.Inbound{
		Source:    tracker.Source,
		EventType: string(kind),
		Sender:    "ana",
		Subject:   "ENG-1 " + string(kind) + ": wire it",
		Metadata: map[string]string{
			tracker.MetaVia:        string(reason),
			tracker.MetaTaskKey:    "ENG-1",
			tracker.MetaTaskID:     "t-1",
			tracker.MetaProject:    "ENG",
			tracker.MetaChangeKind: string(kind),
			tracker.MetaStatus:     "in_progress",
			tracker.MetaAssignee:   "cy",
		},
	}
}
