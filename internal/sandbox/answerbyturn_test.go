package sandbox

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: sweID, Role: "SWE",
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
	rig.ask(turnID, Ask{Question: "which branch?", To: AskToManager})
	rig.runner.Finish(Result{})
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
		TurnID: turnID, Agent: sweID, AgentHandle: "swe", Answer: "use the release branch",
		AnsweredBy: "founder", AnsweredByKind: "human", OperatorID: "pat:founder-token",
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

// asksAgain is a resumed turn that relaunches its executor, and the new job
// parks on a question of its own: the run waits on a NEW question, under a new
// launch, asked an hour after the first.
func asksAgain(t *testing.T, rig *coordRig) func(context.Context, PendingRun) {
	return func(ctx context.Context, run PendingRun) {
		if _, err := rig.pending.BeginLaunch(ctx, run, Fence{}); err != nil {
			t.Errorf("relaunch: %v", err)
		}
		rig.suspendIn(ctx, run.TurnID)
		relaunched := rig.getIn(ctx, run.TurnID)
		if got, err := rig.pending.RecordAsk(ctx, run.TurnID, relaunched.LaunchID,
			Ask{Question: "which test suite?", To: AskToRequester}); err != nil || got != AskRecorded {
			t.Errorf("the second question = %q, %v, want it recorded", got, err)
		}
		if err := rig.pending.MarkAwaiting(ctx, run.TurnID, Clarification{
			AskedAt: rig.now.Add(time.Hour),
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
	t.Parallel()
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
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")

	given, ev := givenAgainst(t, rig, "t1")
	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, %v, want consumed", disposition, err)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered {
		t.Fatalf("run = %q once the answer arrived, want it answered at once: the record "+
			"needs nothing from the seat but its lease", got.Status)
	}
	if rig.fireRetries() != 1 {
		t.Fatal("the recorded answer's resume was not queued")
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("the run was resumed %d times, want once", len(calls))
	}
	if asked := askedOf(calls[0]); asked.By != "founder" || asked.Answer != "use the release branch" ||
		calls[0].Answer != "" {
		t.Errorf("the resumed turn carries the run on with %+v and files %q as its report, "+
			"want the answer attributed to the person who gave it, and no report",
			asked, calls[0].Answer)
	}
	if calls[0].Trigger == nil || calls[0].Trigger.ID != ev.ID {
		t.Error("the resume is not traced to the answer that drove it")
	}
	rig.finished("t1")

	records := rig.answeredRecords()
	if len(records) != 1 {
		t.Fatalf("published %d sandbox_run_answered, want one", len(records))
	}
	got := records[0]
	got.AskedAt, got.AudienceHandles, got.LaunchID = time.Time{}, nil, ""
	want := types.SandboxRunAnswered{
		Agent: sweID, AgentHandle: "swe", RoleName: "SWE", TurnID: "t1",
		Via: types.AnswerViaOperator, Outcome: types.AnswerResumed,
		AnsweredBy: "founder", AnsweredByKind: "human", OperatorID: "pat:founder-token",
		Question: "which branch?", Answer: "use the release branch",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sandbox_run_answered = %+v, want %+v", got, want)
	}
	if records[0].LaunchID == "" || records[0].AskedAt.IsZero() {
		t.Errorf("the record names no question it answered: launch %q, asked at %v",
			records[0].LaunchID, records[0].AskedAt)
	}
}

// parksOnADecision is [parksOnAQuestion] for a run that asks a person to
// choose: its ask carries a decision whose options the answer names by id.
func parksOnADecision(t *testing.T, rig *coordRig, turnID string) {
	t.Helper()
	const question = "which branch?"
	rig.ask(turnID, Ask{Question: question, Decision: choosing(question), To: AskToManager})
	rig.runner.Finish(Result{})
	payload, ev := rig.completion(turnID)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get(turnID); got.Status != StatusAwaiting || got.Decision == nil {
		t.Fatalf("the run is %q with decision %+v after asking, want it parked on its decision",
			got.Status, got.Decision)
	}
}

// AN ANSWER BY TURN THAT CHOOSES IS RECORDED AS THE OPTION IT CHOSE, as the run
// asked it: the option's label and its id, then the person's words. The run
// reads what it offered, never a bare id it would have to look up.
//
// Mutation: record the answer's words alone, and the run never learns what was
// chosen.
func TestAnAnswerByTurnRecordsTheOptionItChose(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnADecision(t, rig, "t1")

	given, _ := givenAgainst(t, rig, "t1")
	given.Choice, given.Answer = "release-2", "  and cut a patch  "
	ev := events.New(given, events.TraceContext{})
	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, %v, want consumed", disposition, err)
	}
	if rig.fireRetries() != 1 {
		t.Fatal("the recorded answer's resume was not queued")
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("the run was resumed %d times, want once", len(calls))
	}
	const want = "Chosen option: Target release/2 (`release-2`).\n\nand cut a patch"
	if asked := askedOf(calls[0]); asked.Answer != want || asked.By != "founder" {
		t.Errorf("the run is carried on with %+v, want the answer %q from the person who gave it",
			asked, want)
	}

	// AND THE RECORD THAT OUTLIVES THE RUN SAYS THE SAME (ADR-0068): the
	// question with its options, the option chosen by id and the person's own
	// words apart from it, which is what their answered decisions read back.
	rig.finished("t1")
	records := rig.answeredRecords()
	if len(records) != 1 {
		t.Fatalf("published %d sandbox_run_answered, want one", len(records))
	}
	got := records[0]
	if got.Outcome != types.AnswerResumed || got.Choice != "release-2" ||
		got.Answer != "and cut a patch" || got.Decision == nil ||
		len(got.Decision.Options) != 2 || got.Question != "which branch?" {
		t.Errorf("sandbox_run_answered = %+v, want the question, its decision, the choice "+
			"and the person's own words", got)
	}
}

// AN ANSWER BY TURN CHOOSING WHAT THE QUESTION DOES NOT OFFER IS SPENT, NOT
// RECORDED. `answer_run` checks the choice against the same question before it
// delivers, so one arriving here was checked against another; handed to the
// run, it would be told it chose something nobody offered. Nothing is resumed,
// the question stays open for an answer it can take, and the person is told
// on the record that theirs did not land.
//
// Mutation: record the answer whatever its choice, and the run resumes on an
// option it never offered.
func TestAnAnswerByTurnChoosingWhatWasNotOfferedIsSpentUnrecorded(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnADecision(t, rig, "t1")

	given, _ := givenAgainst(t, rig, "t1")
	given.Choice = "develop"
	ev := events.New(given, events.TraceContext{})
	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("AnswerByTurn = %q, %v, want not_mine", disposition, err)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("the run is %q with answer %+v, want it still waiting on its question", got.Status, got.Answer)
	}
	if rig.fireRetries() != 0 || len(rig.resumer.calls()) != 0 {
		t.Fatal("an answer choosing an option nobody offered resumed the run")
	}
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Outcome != types.AnswerNotAwaiting {
		t.Fatalf("recorded %+v, want one not_awaiting", records)
	}

	// And the question still takes an answer that chooses what it offers.
	given, _ = givenAgainst(t, rig, "t1")
	given.Choice = "main"
	ev = events.New(given, events.TraceContext{})
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerConsumed {
		t.Fatalf("an offered choice after it = %q, %v, want consumed", d, err)
	}
}

// AN ANSWER TO A RUN SOMEBODY ALREADY ANSWERED IS NOT ITS. The record is the
// same compare-and-set the chat route's is, so a question another answer was
// recorded against is not answered a second time — and the person who answered
// late is told so on the record rather than left to wonder.
func TestAnAnswerByTurnToARunAlreadyAnsweredIsNotMine(t *testing.T) {
	t.Parallel()
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
// are SEPARATED BY A RELAUNCH — the first resumed the run, its turn relaunched
// its executor and the new job parked on a question of its own before the
// second arrived — the second is still an answer to the FIRST question, and spent
// as `not_awaiting`: it used to claim whatever the run waited on by then,
// resuming it a second time with the first question's answer presented as the
// second's.
func TestTwoAnswersByTurnResumeOnce(t *testing.T) {
	t.Parallel()
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
		rig.fireRetries()

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
		rig.fireRetries()
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
	t.Parallel()
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
// spent by the node holding the seat, so it is refused before anything is read
// or put on the seat's inbox. The run exists, so the answer naming its question
// afterwards is delivered — the desk reads the row for the seat it addresses.
func TestTheAnswerDeskRefusesAnAnswerNamingNoQuestion(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	store := &refusingStore{inner: rig.pending}
	desk := AnswerDesk{Pending: store, Queue: rig.queue}
	given, _ := givenFor("t1")
	before := rig.queue.count()
	if _, err := desk.Deliver(t.Context(), given); err == nil {
		t.Fatal("the desk delivered an answer that names no question")
	}
	if rig.queue.count() != before {
		t.Fatal("the desk published an answer it refused")
	}
	if calls := store.calls(); len(calls) != 0 {
		t.Fatalf("the store was asked %v: an answer naming no question is refused before "+
			"the run is read", calls)
	}
	given.LaunchID = "launch-1"
	if _, err := desk.Deliver(t.Context(), given); err != nil {
		t.Fatalf("the desk refused an answer naming its question: %v", err)
	}
}

// A RUN THAT IS NOT WAITING IS NOT ANSWERED, and a run that is gone is gone:
// neither is resumed, neither is handed back — there is nothing to retry for —
// and each says what it found.
func TestAnAnswerByTurnToARunNotWaitingIsSpentAndSaysWhy(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		!slices.Equal(got.Answer.EventIDs, []string{ev.ID.String()}) {
		t.Fatalf("run %q answer %+v, want the answer by turn recorded on it, owed its resume",
			got.Status, got.Answer)
	}
	if words := openedOn(t, got); words.Via != types.AnswerViaOperator || words.By != "founder" ||
		words.ByKind != "human" || words.OperatorID != "pat:founder-token" {
		t.Fatalf("the recorded answer opens to %+v, want the answer by turn with its author, "+
			"their kind and the credential it came through", words)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("announced %+v for an answer whose resume is still owed", records)
	}

	rig.resumer.failWith(nil)
	if rig.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to retry the recorded answer's resume")
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 || askedOf(calls[0]).Answer != "use the release branch" ||
		askedOf(calls[0]).By != "founder" {
		t.Fatalf("resumed %+v, want the run resumed once with the answer, attributed", calls)
	}
	rig.finished("t1")
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Via != types.AnswerViaOperator || records[0].Outcome != types.AnswerResumed ||
		records[0].AnsweredBy != "founder" || records[0].AnsweredByKind != "human" ||
		records[0].OperatorID != "pat:founder-token" {
		t.Fatalf("announced %+v, want one operator answer that resumed the run", records)
	}
}

// AN ANSWER BY TURN ITS RUN COULD NOT BE RESUMED WITH IS LET GO OF, and the
// person told: past the attempts a recorded answer gets, the run waits on its
// question again and the answer's copy goes back to the seat's control topic, where,
// having no ordinary form, it is spent as `declined`. It is never recorded
// against the question again, so it cannot circle the run it failed to reach.
func TestAnAnswerByTurnItsRunCannotTakeIsDeclined(t *testing.T) {
	t.Parallel()
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
	copied := rig.lastHandedBack(t)
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

// lastHandedBack is the newest reply handed back to the seat ([handedBackPublication]).
func (r *coordRig) lastHandedBack(t *testing.T) *events.Event {
	t.Helper()
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	for i := len(r.queue.published) - 1; i >= 0; i-- {
		if p := r.queue.published[i]; handedBackPublication(p) {
			return p.event
		}
	}
	t.Fatal("nothing was handed back to the seat")
	return nil
}

// THE CHAT ROUTE RECORDS WHO ANSWERED. A reply on the run's own conversation
// was taken as the answer and resumed the run, and nothing said so; now the
// resume is on the record, with the route and the sender.
func TestTheChatAnswerPathRecordsWhoAnswered(t *testing.T) {
	t.Parallel()
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
	if got.AnsweredByKind != "human" || got.OperatorID != "" {
		t.Errorf("a chat answer is recorded as kind %q via %q, want a person and no "+
			"credential: the chat surface vouched for the sender", got.AnsweredByKind, got.OperatorID)
	}
	if got.WorkItem == nil || *got.WorkItem != rigItem {
		t.Errorf("the record names item %v, want the run's %v", got.WorkItem, rigItem)
	}
}

// A CHAT REPLY THAT REACHED NOTHING IS NOT AN ANSWER, and publishes nothing:
// every ordinary message on a seat with a parked run is offered to the match,
// and a record per message would bury the answers that were.
func TestAChatMessageThatMatchedNoRunRecordsNothing(t *testing.T) {
	t.Parallel()
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

// AN ANSWER IS ROUTED BY THE SEAT'S ID. One that reaches a seat that does not
// hold the run is dropped — resuming it would re-enter another seat's
// conversation as this one.
func TestAnAnswerByTurnIsMatchedToItsRunBySeatID(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")

	// Against the question the run waits on, so the seat is the one thing
	// wrong with it: an answer naming no question is spent before the run
	// is read at all.
	given, _ := givenAgainst(t, rig, "t1")
	given.Agent = "0198f3a4-0000-7000-8000-000000000002"
	ev := events.New(given, events.TraceContext{})
	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("AnswerByTurn = %q, %v, want not_mine", disposition, err)
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("an answer addressed to another seat resumed the run %d times", n)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("a misrouted answer was recorded as %+v, want nothing", records)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Errorf("the run is %q, want it still waiting for its own answer", got.Status)
	}
}

// THE DESK ADDRESSES THE RUN'S OWN SEAT. Whatever the caller's copy of the run
// said, the answer goes to the inbox of the seat ID on the row, stamped with
// it, so the dispatcher's own check against the row passes on the holder and
// nowhere else — and the question it answers stays the one the caller named,
// never the one the desk's own read finds.
func TestTheAnswerDeskDeliversToTheRunsSeatID(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	seat := uuid.MustParse("0198f3a4-1c2b-7d3e-8f40-5a6b7c8d9e0f")
	opened, err := rig.pending.BeginLaunch(t.Context(), PendingRun{
		TurnID: "t9", AgentHandle: "swe", AgentID: seat.String(), Role: "SWE",
		CodingAgent: "claude-code", CreatedAt: rig.now,
	}, Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	desk := AnswerDesk{Pending: rig.pending, Queue: rig.queue}

	given := types.SandboxAnswerGiven{
		TurnID: "t9", Agent: "a-stale", AgentHandle: "old-handle", Answer: "use main",
		AnsweredBy: "founder", AnsweredByKind: "human", LaunchID: "the-question-the-person-read",
	}
	id, err := desk.Deliver(t.Context(), given)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	var delivered []publication
	for _, p := range rig.queue.published {
		if _, ok := p.event.Data.(*types.SandboxAnswerGiven); ok {
			delivered = append(delivered, p)
		}
	}
	if len(delivered) != 1 {
		t.Fatalf("published %d answers, want one", len(delivered))
	}
	if delivered[0].event.ID != id {
		t.Errorf("Deliver answered id %s for the delivery %s: the caller reads the run's "+
			"record against it", id, delivered[0].event.ID)
	}
	if want := topics.AgentControl(seat); delivered[0].topic != want {
		t.Errorf("the answer went to %q, want the run's seat's control topic %q: on its "+
			"inbox it waits behind every job that holds the seat and every message before it",
			delivered[0].topic, want)
	}
	got := delivered[0].event.Data.(*types.SandboxAnswerGiven)
	if got.Agent != seat.String() {
		t.Errorf("the answer is stamped for seat %q, want the row's %q", got.Agent, seat)
	}
	if got.LaunchID != given.LaunchID || got.LaunchID == opened.LaunchID {
		t.Errorf("the answer names question %q, want the caller's %q and never the row's "+
			"current %q", got.LaunchID, given.LaunchID, opened.LaunchID)
	}

	if _, err := desk.Deliver(t.Context(), types.SandboxAnswerGiven{
		TurnID: "gone", Answer: "x", LaunchID: "x",
	}); err == nil {
		t.Error("an answer to a run that has no row was delivered somewhere")
	}
}

// AN ANSWER BY TURN SURVIVES ITS CLAIM'S NODE STOPPING. It is recorded on the
// run before the resume claims it, so a node that stops between the claim and
// the turn leaves the answer on the row for the seat's next holder — never a
// claim that is reaped as an abandoned tail with nothing on it, which is how
// the answer was lost: it was held only by the claim, and its delivery, coming
// round to the new holder, found the run gone. The next holder gives it back to
// the run and resumes the run with it, attributed to the person who gave it, as
// it does a chat reply's ([Coordinator.reapTail]).
func TestAnAnswerByTurnOutlivesItsClaimsNode(t *testing.T) {
	t.Parallel()
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
	revivedUnder(t, rig, 2, 1)
	if got := openedOn(t, rig.get("t1")); got.Via != types.AnswerViaOperator || got.By != given.AnsweredBy ||
		got.ByKind != given.AnsweredByKind || got.OperatorID != given.OperatorID {
		t.Fatalf("revived answer %+v, want the answer by turn with who gave it", got)
	}
	next.fireRetries()
	resumedOnceWith(t, rig, next, given.Answer)
	if calls := next.resumer.calls(); askedOf(calls[0]).By != given.AnsweredBy {
		t.Fatalf("resumed with %+v, want the answer attributed to %q", askedOf(calls[0]), given.AnsweredBy)
	}
}

// recordLandsThenFails records an answer and then reports that it could not:
// the write committed, and the store's reply was lost.
type recordLandsThenFails struct{ PendingStore }

func (s recordLandsThenFails) RecordAnswer(ctx context.Context, turnID, launch string, answer RecordedAnswer,
	fence Fence,
) (PendingRun, bool, error) {
	if _, ok, err := s.PendingStore.RecordAnswer(ctx, turnID, launch, answer, fence); err != nil || !ok {
		return PendingRun{}, ok, err
	}
	return PendingRun{}, false, errors.New("the coordination store's reply to the record was lost")
}

// resumedOnceHeld asserts the run was resumed exactly once, with the seat's
// inbox held through the resume, and that nothing further is owed.
func resumedOnceHeld(t *testing.T, rig *coordRig, heldDuring *[]bool) {
	t.Helper()
	rig.fireRetries()
	if calls := rig.resumer.calls(); len(calls) != 1 {
		t.Fatalf("resumed %d times, want the run resumed once with the answer its record holds", len(calls))
	}
	if !slices.Equal(*heldDuring, []bool{true}) {
		t.Fatalf("the seat's inbox held during the resumes: %v, want it held through the one", *heldDuring)
	}
	if rig.coordinator.owes("t1") {
		t.Fatal("still owes the answer after resuming with it")
	}
}

// heldThroughResumes records, for each resume, whether the seat's inbox was
// held while it ran.
func heldThroughResumes(rig *coordRig) *[]bool {
	holds := rig.withHold()
	var held []bool
	rig.resumer.during = func(context.Context, PendingRun) { held = append(held, holds.holding("swe")) }
	return &held
}

// AN ANSWER BY TURN WHOSE RECORD LANDED UNSEEN STARTS ITS RESUME WHEN IT COMES
// BACK. The record committed and its reply was lost, so the delivery was handed
// back and nothing was scheduled: the run is answered, owed a resume nothing on
// this node drives, and nothing holds the seat's mail behind it. The redelivery
// finds this very answer on the run, is spent as it — and starts the resume, the
// seat's inbox held through it. Without that, the person's answer waited on the
// run until the seat changed hands.
func TestAnAnswerByTurnWhoseRecordLandedUnseenStartsItsResumeWhenItComesBack(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	heldDuring := heldThroughResumes(rig)
	given, ev := givenAgainst(t, rig, "t1")
	plain := rig.coordinator.pending
	rig.coordinator.pending = recordLandsThenFails{plain}

	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerDeferred || err == nil {
		t.Fatalf("the first delivery = %q, %v, want it handed back with the store's failure", d, err)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil ||
		!slices.Contains(got.Answer.EventIDs, ev.ID.String()) {
		t.Fatalf("run %q with answer %+v, want the premise: the record landed", got.Status, got.Answer)
	}
	if calls := rig.resumer.calls(); len(calls) != 0 || rig.coordinator.owes("t1") {
		t.Fatalf("resumed %d times, owes %v, after a record the store reported as failed",
			len(calls), rig.coordinator.owes("t1"))
	}

	rig.coordinator.pending = plain
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerConsumed || err != nil {
		t.Fatalf("the redelivery = %q, %v, want it spent as the answer it already is", d, err)
	}
	resumedOnceHeld(t, rig, heldDuring)
}

// AND THE SAME ON THE CHAT ROUTE: a reply whose record landed unseen comes back,
// is recognised as the answer on the run, and starts the resume.
func TestAReplyWhoseRecordLandedUnseenStartsItsResumeWhenItComesBack(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	heldDuring := heldThroughResumes(rig)
	reply := chatReply(answerOnTheDM, "use main", replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute)))
	plain := rig.coordinator.pending
	rig.coordinator.pending = recordLandsThenFails{plain}

	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); d != AnswerDeferred || err == nil {
		t.Fatalf("the first delivery = %q, %v, want it handed back with the store's failure", d, err)
	}
	if got := rig.get("t1"); got.Status != StatusAnswered || got.Answer == nil {
		t.Fatalf("run %q with answer %+v, want the premise: the record landed", got.Status, got.Answer)
	}
	if calls := rig.resumer.calls(); len(calls) != 0 || rig.coordinator.owes("t1") {
		t.Fatalf("resumed %d times, owes %v, after a record the store reported as failed",
			len(calls), rig.coordinator.owes("t1"))
	}

	rig.coordinator.pending = plain
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", reply); d != AnswerConsumed || err != nil {
		t.Fatalf("the redelivery = %q, %v, want it spent as the answer it already is", d, err)
	}
	resumedOnceHeld(t, rig, heldDuring)
}
