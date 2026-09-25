package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

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

// givenFor is an operator's answer to one run.
func givenFor(turnID string) (types.SandboxAnswerGiven, *events.Event) {
	given := types.SandboxAnswerGiven{
		TurnID: turnID, AgentHandle: "swe", Answer: "use the release branch",
		AnsweredBy: "founder-token", AnsweredBySeat: "founder",
	}
	return given, events.New(given, events.TraceContext{})
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

	given, ev := givenFor("t1")
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
	if calls[0].Trigger != ev {
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

// AN ANSWER TO A RUN SOMEBODY ALREADY ANSWERED IS NOT ITS. The claim is the
// same one the chat route takes, so a question another answer holds is not
// resumed a second time — and the person who answered late is told so on the
// record rather than left to wonder.
func TestAnAnswerByTurnToARunAlreadyAnsweredIsNotMine(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	run := rig.get("t1")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", AnswerTail(run.LaunchID)); err != nil || !won {
		t.Fatalf("the first answer's claim = %v, %v", won, err)
	}
	// READ BEFORE THE OTHER ANSWER CLAIMED IT, which is the race the claim
	// exists for: this answer saw the question open, and the claim is what
	// finds it taken.
	rig.coordinator.pending = staleRead{PendingStore: rig.coordinator.pending, snapshot: run}

	given, ev := givenFor("t1")
	disposition, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("AnswerByTurn = %q, %v, want not_mine", disposition, err)
	}
	if n := len(rig.resumer.calls()); n != 0 {
		t.Fatalf("a run another answer holds was resumed %d times", n)
	}
	records := rig.answeredRecords()
	if len(records) != 1 || records[0].Outcome != types.AnswerNotAwaiting {
		t.Fatalf("recorded %+v, want one not_awaiting", records)
	}
	if got := rig.get("t1"); got.Status != StatusResumed {
		t.Errorf("the run is %q, want it still held by the answer that claimed it", got.Status)
	}
}

// staleRead answers Get with a snapshot taken earlier, as a read that lost a
// race to another writer does.
type staleRead struct {
	PendingStore
	snapshot PendingRun
}

func (s staleRead) Get(context.Context, string) (PendingRun, bool, error) {
	return s.snapshot, true, nil
}

// TWO ANSWERS AT ONCE RESUME THE RUN ONCE. Two people answering the same
// question — or one person's retry racing their own first try — contend for
// one claim, and the loser is spent rather than resumed or handed back.
func TestTwoAnswersByTurnResumeOnce(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")

	var wg sync.WaitGroup
	dispositions := make([]AnswerDisposition, 2)
	for i := range dispositions {
		wg.Go(func() {
			given, ev := givenFor("t1")
			d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
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
}

// A RUN THAT IS NOT WAITING IS NOT ANSWERED, and a run that is gone is gone:
// neither is resumed, neither is handed back — there is nothing to retry for —
// and each says what it found.
func TestAnAnswerByTurnToARunNotWaitingIsSpentAndSaysWhy(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "running")

	given, ev := givenFor("running")
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerNotMine {
		t.Fatalf("answering a running job = %q, %v, want not_mine", d, err)
	}
	given, ev = givenFor("never-was")
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

	given, ev := givenFor("t1")
	d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev)
	if d != AnswerDeferred || err == nil {
		t.Fatalf("AnswerByTurn over an unreadable store = %q, %v, want deferred with the cause", d, err)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("an answer still owed a retry was announced as %+v", records)
	}
}

// A RESUME THAT FAILED HANDS THE ANSWER BACK, with the claim given back, so the
// run is awaiting this very answer again — never spent on a failure a retry
// can clear, and never announced.
func TestAnAnswerByTurnWhoseResumeFailedComesBack(t *testing.T) {
	rig := newCoordRig(t)
	launchScheduled(t, rig, "t1")
	parksOnAQuestion(t, rig, "t1")
	rig.resumer.failWith(errors.New("the node lost the seat"))

	given, ev := givenFor("t1")
	if d, _ := rig.coordinator.AnswerByTurn(t.Context(), given, ev); d != AnswerDeferred {
		t.Fatalf("AnswerByTurn with a failing resume = %q, want deferred", d)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting {
		t.Fatalf("the run is %q, want it back awaiting the answer", got.Status)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("announced %+v for an answer that is coming back", records)
	}
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
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", answerOnTheDM, "use main", reply)
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
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", elsewhere, "hello", nil)
	if err != nil || d != AnswerNotMine {
		t.Fatalf("TryResumeFromAnswer = %q, %v, want not_mine", d, err)
	}
	if records := rig.answeredRecords(); len(records) != 0 {
		t.Fatalf("a message that answered nothing was recorded as %+v", records)
	}
}
