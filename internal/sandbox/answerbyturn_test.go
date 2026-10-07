package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The second way an answer reaches a parked run — named by its turn — and the
// record both ways leave.

// launchScheduled starts a run the way a schedule's, an assignment's or a
// colleague's turn does: with NO conversation at all, because its trigger named
// none. The run the chat route can never answer.
func launchScheduled(t *testing.T, rig *coordRig, turnID string) {
	t.Helper()
	box, err := rig.provider.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		CodingAgent: "claude-code", TraceID: "tr-1", CreatedAt: rig.now,
		Requester: "ada",
	}, Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := rig.pending.AttachSandbox(t.Context(), turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
		PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	rig.suspend(turnID)
	rig.coordinator.countRun("swe", StatusRunning)
}

// parksOnAQuestion finishes the run's job on a question and hands the
// coordinator its completion, which parks it.
func parksOnAQuestion(t *testing.T, rig *coordRig, turnID string) {
	t.Helper()
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "manager"})
	payload, ev := rig.completion(turnID)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get(turnID); got.Status != StatusAwaiting {
		t.Fatalf("the run is %q after asking, want it parked", got.Status)
	}
}

// givenFor is an operator's answer to one run that names the run alone and no
// question — which no answer this build delivers does.
func givenFor(turnID string) (types.SandboxAnswerGiven, *events.Event) {
	given := types.SandboxAnswerGiven{
		TurnID: turnID, AgentHandle: "swe", Answer: "use the release branch",
		AnsweredBy: "founder-token", AnsweredBySeat: "founder",
	}
	return given, events.New(given, events.TraceContext{})
}

// givenAgainst is an operator's answer to the question the run is waiting on
// now, as answer_run gives it: the run and its question, off the row.
func givenAgainst(t *testing.T, rig *coordRig, turnID string) (types.SandboxAnswerGiven, *events.Event) {
	t.Helper()
	run := rig.get(turnID)
	given, _ := givenFor(turnID)
	given.LaunchID = run.LaunchID
	return given, events.New(given, events.TraceContext{})
}

// asksAgain is a resumed turn that calls run_sandbox again, and the new job
// parks on a question of its own: the run waits on a NEW question, under a new
// launch, asked an hour after the first.
func asksAgain(t *testing.T, rig *coordRig) func(context.Context, PendingRun) {
	return func(ctx context.Context, run PendingRun) {
		if err := rig.pending.BeginLaunch(ctx, run, Fence{}); err != nil {
			t.Errorf("relaunch: %v", err)
		}
		rig.suspendIn(ctx, run.TurnID)
		if err := rig.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
			Question: "which test suite?", AskedAt: rig.now.Add(time.Hour),
		}); err != nil {
			t.Errorf("the second question: %v", err)
		}
	}
}

// answeredRecords is every sandbox_run_answered the coordinator published.
func (r *coordRig) answeredRecords() []types.SandboxRunAnswered {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []types.SandboxRunAnswered
	for _, p := range r.queue.published {
		if payload, ok := p.event.Data.(*types.SandboxRunAnswered); ok {
			if p.topic != topics.Event(payload.EventType()) {
				continue
			}
			out = append(out, *payload)
		}
	}
	return out
}

// THE PARK PUTS THE QUESTION TO WHOM THE CHART SAYS, and writes it with the
// question. The label the coding agent chose is resolved for the run that
// asked — its requester is on the row — and a row read back carries the seats
// and whether they are a fallback.
func TestTheParkRecordsWhomTheQuestionIsPutTo(t *testing.T) {
	rig := newCoordRig(t)
	rig.audience.resolves(Audience{Handles: []string{"founder", "cto"}, Fallback: true})
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")

	if asked := rig.audience.questions(); !slices.Equal(asked, []string{"t1/manager"}) {
		t.Fatalf("the resolver was asked %v, want the run's own label once", asked)
	}
	got := rig.get("t1")
	if !slices.Equal(got.AudienceHandles, []string{"founder", "cto"}) || !got.AudienceFallback {
		t.Fatalf("the parked row says the question is put to %v (fallback %v), want "+
			"[founder cto] as a fallback", got.AudienceHandles, got.AudienceFallback)
	}
}

// A RUN WITH NO CONVERSATION IS ANSWERED BY ITS TURN. Nothing a person could
// say in chat would reach it — its row names no conversation to match — so the
// answer names the run, resumes the suspended turn with it, attributed, and
// the resume is on the record with who gave it.
func TestAnAnswerByTurnResumesARunWithNoConversation(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")

	given, ev := givenAgainst(t, rig, "t1")
	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, %v, want consumed", disposition, err)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("the run was resumed %d times, want once", len(calls))
	}
	if !strings.Contains(calls[0].Answer, "Answer from founder: use the release branch") {
		t.Errorf("the resumed turn was told %q, want the answer attributed to the "+
			"person who gave it", calls[0].Answer)
	}
	if calls[0].Trigger == nil || calls[0].Trigger.ID != ev.ID {
		t.Error("the resume is not traced to the answer that drove it")
	}
	rig.finished("t1")

	records := rig.answeredRecords()
	if len(records) != 1 {
		t.Fatalf("published %d sandbox_run_answered, want one", len(records))
	}
	want := types.SandboxRunAnswered{
		Agent: "a-1", AgentHandle: "swe", RoleName: "SWE", TurnID: "t1",
		Via: types.AnswerViaOperator, Outcome: types.AnswerResumed,
		AnsweredBy: "founder-token", AnsweredBySeat: "founder",
	}
	if got := records[0]; got != want {
		t.Errorf("sandbox_run_answered = %+v, want %+v", got, want)
	}
}

// AN ANSWER TO A RUN SOMEBODY ALREADY ANSWERED IS NOT ITS. The record is the
// same compare-and-set the chat route's is, so a question another answer was
// recorded against is not answered a second time — and the person who answered
// late is told so on the record rather than left to wonder.
func TestAnAnswerByTurnToARunAlreadyAnsweredIsNotMine(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	late, lateEv := givenAgainst(t, rig, "t1")
	// READ BEFORE THE OTHER ANSWER WAS RECORDED, which is the race the record
	// exists for: this answer saw the question open, and the record is what
	// finds it answered.
	stale := rig.get("t1")
	rig.resumer.failWith(errors.New("the first answer's resume is still being retried"))
	first, firstEv := givenAgainst(t, rig, "t1")
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), first, firstEv); err != nil || d != AnswerConsumed {
		t.Fatalf("the first answer = %q, %v, want it recorded", d, err)
	}
	rig.coordinator.pending = &staleOnce{PendingStore: rig.coordinator.pending, snapshot: stale}

	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), late, lateEv)
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("AnswerByTurn = %q, %v, want not_mine", disposition, err)
	}
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Outcome != types.AnswerNotAwaiting {
		t.Fatalf("recorded %+v, want one not_awaiting", records)
	}
	if got := rig.get("t1"); got.Answer == nil || !slices.Equal(got.Answer.EventIDs, []string{firstEv.ID.String()}) {
		t.Errorf("the run's answer is %+v, want the first one still recorded", got.Answer)
	}
}

// staleOnce answers its first Get with a snapshot taken earlier, as a read that
// lost a race to another writer does, and every later one from the store.
type staleOnce struct {
	PendingStore
	snapshot PendingRun
	read     bool
}

func (s *staleOnce) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if !s.read {
		s.read = true
		return s.snapshot, true, nil
	}
	return s.PendingStore.Get(ctx, turnID)
}

// TWO ANSWERS TO ONE QUESTION RESUME THE RUN ONCE. Two people answering the same
// question — or one person's retry racing their own first try — contend for one
// record, and the loser is spent rather than resumed or handed back; and the
// same answer delivered twice is the one answer, spent as it. And when the two
// are SEPARATED BY A RELAUNCH — the first resumed the run, its turn called
// run_sandbox again and the new job parked on a question of its own before the
// second arrived — the second is still an answer to the FIRST question, and spent
// as `not_awaiting`: it used to claim whatever the run waited on by then,
// resuming it a second time with the first question's answer presented as the
// second's.
func TestTwoAnswersByTurnResumeOnce(t *testing.T) {
	t.Run("two answers at once", func(t *testing.T) {
		rig := newCoordRig(t)
		launchScheduled(t, rig, "t1")
		parksOnAQuestion(t, rig, "t1")
		one, oneEv := givenAgainst(t, rig, "t1")
		other, otherEv := givenAgainst(t, rig, "t1")

		var wg sync.WaitGroup
		dispositions := make([]AnswerDisposition, 2)
		for i, answer := range []struct {
			given types.SandboxAnswerGiven
			ev    *events.Event
		}{{one, oneEv}, {other, otherEv}} {
			wg.Go(func() {
				d, err := rig.coordinator.AnswerByTurn(t.Context(), answer.given, answer.ev)
				if err != nil {
					t.Errorf("AnswerByTurn: %v", err)
				}
				dispositions[i] = d
			})
		}
		wg.Wait()

		if n := len(rig.resumer.calls()); n != 1 {
			t.Fatalf("two answers resumed the run %d times, want exactly once", n)
		}
		slices.Sort(dispositions)
		if !slices.Equal(dispositions, []AnswerDisposition{AnswerConsumed, AnswerNotMine}) {
			t.Fatalf("dispositions = %v, want one consumed and one not_mine", dispositions)
		}
	})
	t.Run("one answer delivered twice", func(t *testing.T) {
		rig := newCoordRig(t)
		launchScheduled(t, rig, "t1")
		parksOnAQuestion(t, rig, "t1")
		rig.resumer.failWith(errors.New("transient"))
		given, ev := givenAgainst(t, rig, "t1")
		for range 2 {
			if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerConsumed {
				t.Fatalf("AnswerByTurn = %q, %v, want consumed: the redelivery is the answer it already is", d, err)
			}
		}
		rig.resumer.failWith(nil)
		rig.fireRetries()
		if n := len(rig.resumer.calls()); n != 1 {
			t.Fatalf("one answer delivered twice resumed the run %d times, want once", n)
		}
	})
	t.Run("separated by a relaunch", func(t *testing.T) {
		rig := newCoordRig(t)
		launchScheduled(t, rig, "t1")
		parksOnAQuestion(t, rig, "t1")
		first, firstEv := givenAgainst(t, rig, "t1")
		late, lateEv := givenAgainst(t, rig, "t1")

		rig.resumer.during = asksAgain(t, rig)
		if d, err := rig.coordinator.AnswerByTurn(t.Context(), first, firstEv); err != nil || d != AnswerConsumed {
			t.Fatalf("the first answer = %q, %v, want it to resume the run", d, err)
		}
		rig.resumer.during = nil
		second := rig.get("t1")
		if second.Status != StatusAwaiting || second.LaunchID == first.LaunchID {
			t.Fatalf("run = %q under %q, want it parked on a NEW question", second.Status, second.LaunchID)
		}

		if d, err := rig.coordinator.AnswerByTurn(t.Context(), late, lateEv); err != nil || d != AnswerNotMine {
			t.Fatalf("the late answer to the first question = %q, %v, want not_mine", d, err)
		}
		if n := len(rig.resumer.calls()); n != 1 {
			t.Fatalf("the run was resumed %d times, want once: the late answer was given against a "+
				"question the run is no longer waiting on", n)
		}
		if got := rig.get("t1"); got.Status != StatusAwaiting || got.LaunchID != second.LaunchID {
			t.Fatalf("run = %q under %q, want the second question still waiting", got.Status, got.LaunchID)
		}
		var outcomes []types.AnswerOutcome
		for _, r := range rig.answeredRecords() {
			outcomes = append(outcomes, r.Outcome)
		}
		if !slices.Equal(outcomes, []types.AnswerOutcome{types.AnswerResumed, types.AnswerNotAwaiting}) {
			t.Fatalf("outcomes = %v, want resumed then not_awaiting", outcomes)
		}
	})
}

// AN ANSWER THAT NAMES NO QUESTION IS SPENT, NOT RESUMED WITH. Which question it
// answers cannot be known: the run may have moved on to a question its giver
// never saw, and resuming it with the answer would pair the two exactly as the
// question's name exists to stop. It is spent with the reason — not handed back,
// since no retry can supply what it lacks — and the run keeps waiting.
func TestAnAnswerByTurnNamingNoQuestionIsSpentUnread(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	store := &refusingStore{inner: rig.coordinator.pending}
	rig.coordinator.pending = store

	unnamed, ev := givenFor("t1")
	d, err := rig.coordinator.AnswerByTurn(t.Context(), unnamed, ev)
	if d != AnswerNotMine || !errors.Is(err, errAnswerNamesNoQuestion) {
		t.Fatalf("an answer naming no question = %q, %v, want it spent with the reason", d, err)
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("resumed %d times with an answer that names no question", n)
	}
	if calls := store.calls(); len(calls) != 0 {
		t.Fatalf("the store was asked %v: an answer naming no question is refused before "+
			"the run is read", calls)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q with answer %+v, want it still waiting on its question", got.Status, got.Answer)
	}
}

// THE DESK WILL NOT DELIVER ONE: an answer naming no question would only be
// spent by the node holding the seat, so it is refused before anything is put
// on the seat's inbox.
func TestTheAnswerDeskRefusesAnAnswerNamingNoQuestion(t *testing.T) {
	rig := newCoordRig(t)
	desk := AnswerDesk{Pending: rig.pending, Queue: rig.queue}
	given, _ := givenFor("t1")
	before := rig.queue.count()
	if err := desk.Deliver(t.Context(), given); err == nil {
		t.Fatal("the desk delivered an answer that names no question")
	}
	if rig.queue.count() != before {
		t.Fatal("the desk published an answer it refused")
	}
	given.LaunchID = "launch-1"
	if err := desk.Deliver(t.Context(), given); err != nil {
		t.Fatalf("the desk refused an answer naming its question: %v", err)
	}
}

// A RUN THAT IS NOT WAITING IS NOT ANSWERED, and a run that is gone is gone:
// neither is resumed, neither is handed back — there is nothing to retry for —
// and each says what it found.
func TestAnAnswerByTurnToARunNotWaitingIsSpentAndSaysWhy(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "running")

	given, ev := givenAgainst(t, rig, "running")
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerNotMine {
		t.Fatalf("answering a running job = %q, %v, want not_mine", d, err)
	}
	given, _ = givenFor("never-was")
	given.LaunchID = "launch-of-a-run-long-over"
	ev = events.New(given, events.TraceContext{})
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerNotMine {
		t.Fatalf("answering a run with no record = %q, %v, want not_mine", d, err)
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("resumed %d times", n)
	}
	var outcomes []types.AnswerOutcome
	for _, r := range rig.answeredRecords() {
		outcomes = append(outcomes, r.Outcome)
	}
	if !slices.Equal(outcomes, []types.AnswerOutcome{types.AnswerNotAwaiting, types.AnswerGone}) {
		t.Fatalf("outcomes = %v, want not_awaiting then gone", outcomes)
	}
}

// A STORE THAT CANNOT BE READ IS NOT A RUN THAT IS GONE. The answer is handed
// back for the broker's spaced retry, and nothing is announced: it has not
// become anything yet.
func TestAnAnswerByTurnOverAnUnreadableStoreComesBack(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	rig.coordinator.pending = &refusingStore{inner: rig.coordinator.pending, refuse: []string{"Get"}}

	given, ev := givenAgainst(t, rig, "t1")
	d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if d != AnswerDeferred || err == nil {
		t.Fatalf("AnswerByTurn over an unreadable store = %q, %v, want deferred with the cause", d, err)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("an answer still owed a retry was announced as %+v", records)
	}
}

// AN ANSWER BY TURN WHOSE RESUME FAILED IS THE RUN'S TO RETRY, as a chat reply's
// is: it is recorded on the run before the resume is attempted, so the delivery
// is spent, the run owes the resume, and the coordinator retries it — nothing
// announced until it has become something, then announced once, by its route.
// It used to be handed back to the broker instead, held only by the claim the
// resume took, so a node that stopped between that claim and the turn lost it.
func TestAnAnswerByTurnWhoseResumeFailedIsRetriedFromItsRecord(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	rig.resumer.failWith(errors.New("the node lost the seat"))

	given, ev := givenAgainst(t, rig, "t1")
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerConsumed {
		t.Fatalf("AnswerByTurn with a failing resume = %q, %v, want consumed: the answer is recorded", d, err)
	}
	got := rig.get("t1")
	if got.Status != StatusAnswered || got.Answer == nil || got.Answer.Via != types.AnswerViaOperator ||
		got.Answer.By != "founder-token" || got.Answer.BySeat != "founder" ||
		!slices.Equal(got.Answer.EventIDs, []string{ev.ID.String()}) {
		t.Fatalf("run %q answer %+v, want the answer by turn recorded on it, owed its resume",
			got.Status, got.Answer)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("announced %+v for an answer whose resume is still owed", records)
	}

	rig.resumer.failWith(nil)
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to retry the recorded answer's resume")
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 || !strings.Contains(calls[0].Answer, "Answer from founder: use the release branch") {
		t.Fatalf("resumed %+v, want the run resumed once with the answer, attributed", calls)
	}
	rig.finished("t1")
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Via != types.AnswerViaOperator || records[0].Outcome != types.AnswerResumed ||
		records[0].AnsweredBy != "founder-token" || records[0].AnsweredBySeat != "founder" {
		t.Fatalf("announced %+v, want one operator answer that resumed the run", records)
	}
}

// AN ANSWER BY TURN ITS RUN COULD NOT BE RESUMED WITH IS LET GO OF, and the
// person told: past the attempts a recorded answer gets, the run waits on its
// question again and the answer's copy goes back to the seat's inbox — where,
// having no ordinary form, it is spent as `declined`. It is never recorded
// against the question again, so it cannot circle the run it failed to reach.
func TestAnAnswerByTurnItsRunCannotTakeIsDeclined(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	rig.resumer.failWith(errors.New("this node cannot decode the conversation"))
	given, ev := givenAgainst(t, rig, "t1")
	if d, _ := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, want the answer recorded", d)
	}
	for range MaxAnswerAttempts {
		rig.fireRetries()
	}
	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q answer %+v, want it back on its question with the answer let go of", got.Status, got.Answer)
	}
	handed := rig.handedBack()
	if len(handed) != 1 || handed[0] != declinedCopyID(ev.ID).String() {
		t.Fatalf("handed back %v, want the answer's copy once", handed)
	}
	copied := rig.lastInboxEvent(t)
	answer, ok := events.DataAs[*types.SandboxAnswerGiven](copied)
	if !ok {
		t.Fatalf("the copy handed back is %T, want the answer by turn it was", copied.Data)
	}
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), *answer, copied); err != nil || d != AnswerNotMine {
		t.Fatalf("the copy = %q, %v, want it spent", d, err)
	}
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerNotMine {
		t.Fatalf("the original come round again = %q, %v, want it spent", d, err)
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("resumed %d times with an answer the run could not take", n)
	}
	var outcomes []types.AnswerOutcome
	for _, r := range rig.answeredRecords() {
		outcomes = append(outcomes, r.Outcome)
	}
	if !slices.Equal(outcomes, []types.AnswerOutcome{types.AnswerDeclined}) {
		t.Fatalf("outcomes = %v, want the person told once, by the copy, that the answer was declined",
			outcomes)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run %q answer %+v, want it still waiting on its question", got.Status, got.Answer)
	}
}

// lastInboxEvent is the newest event published to the seat's inbox.
func (r *coordRig) lastInboxEvent(t *testing.T) *events.Event {
	t.Helper()
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	for i := len(r.queue.published) - 1; i >= 0; i-- {
		if p := r.queue.published[i]; p.topic == topics.AgentInbox("swe") {
			return p.event
		}
	}
	t.Fatal("nothing was published to the seat's inbox")
	return nil
}

// THE CHAT ROUTE RECORDS WHO ANSWERED. A reply on the run's own conversation
// was taken as the answer and resumed the run, and nothing said so; now the
// resume is on the record, with the route and the sender.
func TestTheChatAnswerPathRecordsWhoAnswered(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	parksOnAQuestion(t, rig, "t1")

	reply := events.New(types.ExternalNotification{
		NotificationSource: "slack", Sender: "Ada", Body: "use main",
	}, events.TraceContext{})
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(answerOnTheDM, "use main", reply))
	if err != nil || d != AnswerConsumed {
		t.Fatalf("TryResumeFromAnswer = %q, %v", d, err)
	}
	records := rig.answeredRecords()
	if len(records) != 1 {
		t.Fatalf("published %d sandbox_run_answered, want one", len(records))
	}
	got := records[0]
	if got.Via != types.AnswerViaChat || got.Outcome != types.AnswerResumed ||
		got.TurnID != "t1" || got.AnsweredBy != reply.Actor() || got.AnsweredBy == "" {
		t.Fatalf("recorded %+v, want a chat resume answered by %q", got, reply.Actor())
	}
	if got.WorkItem == nil || *got.WorkItem != rigItem {
		t.Errorf("the record names item %v, want the run's %v", got.WorkItem, rigItem)
	}
}

// A CHAT REPLY THAT REACHED NOTHING IS NOT AN ANSWER, and publishes nothing:
// every ordinary message on a seat with a parked run is offered to the match,
// and a record per message would bury the answers that were.
func TestAChatMessageThatMatchedNoRunRecordsNothing(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	parksOnAQuestion(t, rig, "t1")

	elsewhere := ConversationRef{Identity: "chat:D9", Partition: "chat:D9"}
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", chatReply(elsewhere, "hello", nil))
	if err != nil || d != AnswerNotMine {
		t.Fatalf("TryResumeFromAnswer = %q, %v, want not_mine", d, err)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("a message that answered nothing was recorded as %+v", records)
	}
}

// AN ANSWER BY TURN SURVIVES ITS CLAIM'S NODE STOPPING. It is recorded on the
// run before the resume claims it, so a node that stops between the claim and
// the turn leaves the answer on the row for the seat's next holder — never a
// claim that is reaped as an abandoned tail with nothing on it, which is how
// the answer was lost: it was held only by the claim, and its delivery, coming
// round to the new holder, found the run gone.
func TestAnAnswerByTurnOutlivesItsClaimsNode(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	rig.resumer.failWith(errors.New("transient"))
	given, ev := givenAgainst(t, rig, "t1")
	if d, _ := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, want the answer recorded", d)
	}
	launch := rig.get("t1").LaunchID
	// THE NODE CLAIMS THE RUN FOR THE ANSWER'S RESUME, AND STOPS before its
	// turn takes the answer.
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", RecordedAnswerTail(launch), rigLease); err != nil || !won {
		t.Fatalf("the dying node's claim = %v, %v", won, err)
	}
	rig.coordinator.Stop()
	if got := rig.get("t1"); got.Answer == nil || got.Answer.Taken() ||
		!slices.Equal(got.Answer.EventIDs, []string{ev.ID.String()}) {
		t.Fatalf("the claimed run's answer is %+v, want the answer by turn on it, untaken", got.Answer)
	}

	next := reaper(t.Context(), t, rig, rig.pending, 2, nil)
	next.fireRetries()
	resumed := next.resumer.calls()
	handed := rig.handedBack()
	if len(resumed)+len(handed) != 1 {
		t.Fatalf("the seat's next holder resumed %d times and handed back %v: want the answer to "+
			"reach the seat exactly once, not be lost with the claim", len(resumed), handed)
	}
}
