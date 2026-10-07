package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// chatReply is a chat delivery offered as an answer: the conversation it came
// on, its text, and the event that carried it — a fresh one, posted now, for a
// case whose subject is not the delivery's identity or its instant. A
// delivery always carries an event, and its id is what the answer is
// recorded under.
func chatReply(conv ConversationRef, text string, trigger *events.Event) Reply {
	if trigger == nil {
		trigger = answerFrom(text)
	}
	return Reply{Conv: conv, Text: text, Events: []*events.Event{trigger}}
}

// replyAt is [answerFrom] posted at a given instant.
func replyAt(text string, at time.Time) *events.Event {
	ev := answerFrom(text)
	ev.Timestamp = at
	return ev
}

// scheduled is a hand-cranked [time.AfterFunc]: what the coordinator schedules
// runs only when a test fires it, so a retry's backoff is a step a case takes
// rather than a wait it sits out.
type scheduled struct {
	mu      sync.Mutex
	pending []*scheduledCall
}

type scheduledCall struct {
	f        func()
	delay    time.Duration
	finished bool
}

func (s *scheduled) after(d time.Duration, f func()) func() bool {
	call := &scheduledCall{f: f, delay: d}
	s.mu.Lock()
	s.pending = append(s.pending, call)
	s.mu.Unlock()
	return func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if call.finished {
			return false
		}
		call.finished = true
		return true
	}
}

// fire runs every call scheduled and not cancelled, in order, and reports how
// many ran. A call scheduled while these run waits for the next fire.
func (s *scheduled) fire() int {
	s.mu.Lock()
	var due []*scheduledCall
	for _, call := range s.pending {
		if !call.finished {
			call.finished = true
			due = append(due, call)
		}
	}
	s.pending = nil
	s.mu.Unlock()
	for _, call := range due {
		call.f()
	}
	return len(due)
}

// delays are the backoffs of the calls still waiting to fire.
func (s *scheduled) delays() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Duration
	for _, call := range s.pending {
		if !call.finished {
			out = append(out, call.delay)
		}
	}
	return out
}

// holdSpy records the seat-inbox holds an owed answer takes.
type holdSpy struct {
	mu   sync.Mutex
	held map[string]bool
	log  []string
}

func (h *holdSpy) Hold(_ context.Context, handle string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.held == nil {
		h.held = map[string]bool{}
	}
	h.held[handle] = true
	h.log = append(h.log, "hold "+handle)
	return nil
}

func (h *holdSpy) Release(_ context.Context, handle string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.held, handle)
	h.log = append(h.log, "release "+handle)
	return nil
}

func (h *holdSpy) holding(handle string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held[handle]
}

// withHold gives the rig's coordinator an inbox hold to take.
func (r *coordRig) withHold() *holdSpy {
	spy := &holdSpy{}
	r.coordinator.hold = spy
	return spy
}

// THE R1/R2 RACE, ON THE COORDINATOR. A run parks on a question; the first
// reply R1 arrives and the resume fails on something transient; the person's
// next message R2 arrives before anything retries. The question has ONE
// answer and it is R1: recorded the moment it arrived, so R2 finds the
// question answered and is the ordinary message it looks like, and the retry
// resumes the run with R1 — not with R2, and not with nothing.
//
// It used to go the other way: R1 was handed back to the inbox with a Nak,
// which on the shipped broker returns it BEHIND R2, and the positional match
// gave R2 the question.
func TestTheFirstQualifyingReplyIsTheAnswerWhateverTheResumeDoes(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	if asked.IsZero() {
		t.Fatal("the park recorded no instant for the question, so nothing can tell " +
			"a reply to it from a message written before it")
	}

	rig.resumer.failWith(errors.New("the model provider did not answer"))
	r1 := replyAt("use main", asked.Add(time.Minute))
	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1))
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("R1 = %q, %v, want %q: the reply is recorded on the run before the "+
			"resume is tried, so a resume that fails is the coordinator's to retry "+
			"and never a reason to hand the person's message back", disposition, err, AnswerConsumed)
	}
	got := rig.get("t1")
	if got.Status != StatusAnswered || got.Answer == nil || got.Answer.Text != "use main" {
		t.Fatalf("run = %q with answer %+v, want R1 recorded and its resume owed",
			got.Status, got.Answer)
	}
	if !holds.holding("swe") {
		t.Fatal("the seat's inbox is not held while R1's resume is owed, so R2 would " +
			"be worked as a turn before the run it follows up on")
	}

	r2 := replyAt("actually, also update the docs", asked.Add(2*time.Minute))
	disposition, err = rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "actually, also update the docs", r2))
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("R2 = %q, %v, want %q: the question already has its answer",
			disposition, err, AnswerNotMine)
	}

	rig.resumer.failWith(nil)
	if fired := rig.fireRetries(); fired != 1 {
		t.Fatalf("%d retries fired, want the one R1's failed resume scheduled", fired)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("%d resumes went through, want 1", len(calls))
	}
	if !strings.Contains(calls[0].Answer, "use main") || strings.Contains(calls[0].Answer, "docs") {
		t.Fatalf("the run was resumed with %q, want R1's answer and nothing of R2's",
			calls[0].Answer)
	}
	if calls[0].Trigger == nil || calls[0].Trigger.ID != r1.ID {
		t.Fatalf("the resume was raised off %v, want R1 itself", calls[0].Trigger)
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held after the answer's resume returned")
	}
}

// A REPLY WRITTEN BEFORE THE QUESTION IS NOT ITS ANSWER. A message sent while
// the job was still running sat on the seat's inbox behind the held seat, and
// the moment the job parked it was the "next inbound on the conversation" —
// the answer to a question that did not exist when it was written.
func TestAReplyPostedBeforeTheQuestionDoesNotAnswerIt(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt

	early := replyAt("how is it going?", asked.Add(-time.Second))
	disposition, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "how is it going?", early))
	if err != nil || disposition != AnswerNotMine {
		t.Fatalf("disposition = %q, %v, want %q", disposition, err, AnswerNotMine)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run = %q with answer %+v, want it still waiting on its question",
			got.Status, got.Answer)
	}

	// THE BATCH IS THE REPLY WHEN ITS NEWEST LINE IS: the earlier line rides
	// along as the context it was sent with.
	reply := replyAt("use main", asked.Add(time.Second))
	disposition, err = rig.coordinator.TryResumeFromAnswer(t.Context(), "swe", Reply{
		Conv: answerOnTheDM, Text: "how is it going?\n\nuse main",
		Events: []*events.Event{early, reply},
	})
	if err != nil || disposition != AnswerConsumed {
		t.Fatalf("disposition = %q, %v, want %q", disposition, err, AnswerConsumed)
	}
}

// R1 CANNOT ANSWER A LATER QUESTION. The run resumes with R1, calls
// run_sandbox again and parks on a second question; a copy of R1 arriving
// after that — a redelivery whose acknowledgement was lost — was written
// before the second question was asked, so it is not its answer.
func TestAnAnswerIsNeverTheAnswerToTheNextQuestion(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	r1 := replyAt("use main", asked.Add(time.Minute))

	// The resumed turn launches the next job and it parks on its own
	// question, an hour later.
	rig.resumer.during = func(ctx context.Context, _ PendingRun) {
		rig.resumer.during = nil
		rig.now = rig.now.Add(time.Hour)
		if _, err := rig.launchVia(ctx, rig.manager, launchReq("t1")); err != nil {
			t.Errorf("relaunch: %v", err)
			return
		}
		if ok, err := rig.pending.MarkSuspended(ctx, "t1", Suspension{
			State: json.RawMessage(`{"version":1,"pending_tool_call_id":"call-2","pending_tool_name":"run_sandbox"}`),
		}); err != nil || !ok {
			t.Errorf("MarkSuspended = %v, %v", ok, err)
		}
	}
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); err != nil || d != AnswerConsumed {
		t.Fatalf("R1 = %q, %v, want it to answer the first question", d, err)
	}
	rig.runner.Finish(Result{NeedsInput: true, Question: "which test suite?", AskTo: "requester"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	second := rig.get("t1")
	if second.Status != StatusAwaiting || !second.AskedAt.After(r1.Timestamp) {
		t.Fatalf("run = %q asked at %v, want it parked on a question asked after R1",
			second.Status, second.AskedAt)
	}

	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1))
	if err != nil || d != AnswerNotMine {
		t.Fatalf("a copy of R1 = %q, %v, want %q: R1 was written before the second "+
			"question was asked", d, err, AnswerNotMine)
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run = %q with answer %+v, want the second question still open",
			got.Status, got.Answer)
	}
}

// A COPY OF AN ANSWER IS THAT ANSWER. The node that recorded it may not have
// acknowledged the delivery, so the same delivery can reach the seat again
// while the run still owes its resume: it is spent as the answer it is —
// never matched again, never a turn.
func TestACopyOfARecordedAnswerIsSpentAsThatAnswer(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	r1 := replyAt("use main", asked.Add(time.Minute))
	rig.resumer.failWith(errors.New("no runner on this node"))
	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1))
	if err != nil || d != AnswerConsumed {
		t.Fatalf("the copy = %q, %v, want %q", d, err, AnswerConsumed)
	}
	if got := rig.get("t1"); got.Answer == nil || len(got.Answer.EventIDs) != 1 {
		t.Fatalf("answer = %+v, want R1 recorded once", got.Answer)
	}
}

// THE COORDINATOR'S OWN BACKOFF, AND ITS OWN BOUND. A recorded answer's resume
// is retried a second after it fails, doubling to thirty, and after
// [MaxAnswerAttempts] the answer is let go: the reply is handed to the seat as
// the ordinary message it is (under a new id, because the original is spent),
// the run waits on its question again, and that reply never answers it again.
func TestAnAnswerThatCannotBeResumedIsHandedBackAfterItsAttempts(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	r1 := replyAt("use main", asked.Add(time.Minute))
	rig.resumer.failWith(errors.New("this build cannot read the suspended conversation"))

	if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
		t.Fatalf("R1 = %q, want it recorded", d)
	}
	var delays []time.Duration
	for range MaxAnswerAttempts - 1 {
		delays = append(delays, rig.retries.delays()...)
		if rig.fireRetries() != 1 {
			t.Fatalf("after %d delays, no retry was scheduled: %v", len(delays), delays)
		}
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(delays, want) {
		t.Fatalf("retries were spaced %v, want %v", delays, want)
	}
	if left := rig.retries.delays(); len(left) != 0 {
		t.Fatalf("a retry is still scheduled after %d attempts: %v", MaxAnswerAttempts, left)
	}

	got := rig.get("t1")
	if got.Status != StatusAwaiting || got.Answer != nil {
		t.Fatalf("run = %q with answer %+v, want it waiting on its question again",
			got.Status, got.Answer)
	}
	copyID := declinedCopyID(r1.ID)
	if !slices.Contains(got.DeclinedAnswers, r1.ID.String()) ||
		!slices.Contains(got.DeclinedAnswers, copyID.String()) {
		t.Fatalf("declined = %v, want R1 and its copy", got.DeclinedAnswers)
	}
	var handed []*events.Event
	for _, p := range rig.queue.published {
		if p.topic == topics.AgentInbox("swe") {
			handed = append(handed, p.event)
		}
	}
	if len(handed) != 1 || handed[0].ID != copyID || !handed[0].Timestamp.Equal(r1.Timestamp) {
		t.Fatalf("handed back %v, want one copy of R1 under its derived id", handed)
	}
	if len(got.HandBack) != 0 {
		t.Fatalf("hand-back %+v still owed after the copy was published", got.HandBack)
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held after the answer was let go")
	}
	if d, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
		chatReply(answerOnTheDM, "use main", handed[0])); err != nil || d != AnswerNotMine {
		t.Fatalf("the handed-back copy = %q, %v, want %q: it is the ordinary message now",
			d, err, AnswerNotMine)
	}
}

// AN ANSWER OUTLIVES THE NODE THAT RECORDED IT. The seat's next holder finds
// the run answered and owing its resume, and drives it — with the seat's
// inbox held behind it — rather than waiting for the person to answer twice.
func TestAnAnswerARecoveredSeatOwesIsResumedByItsNewHolder(t *testing.T) {
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	asked := rig.get("t1").AskedAt
	run := rig.get("t1")
	r1 := replyAt("use main", asked.Add(time.Minute))
	if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
		chatReply(answerOnTheDM, "use main", r1).answerOf(rig.now)); err != nil || !won {
		t.Fatalf("RecordAnswer = %v, %v", won, err)
	}

	successor := newCoordRig(t)
	successor.waiterRig = rig.waiterRig
	successor.coordinator.pending = rig.pending
	holds := successor.withHold()
	if err := successor.coordinator.RecoverSeat(t.Context(), "swe", "node-b", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if !holds.holding("swe") {
		t.Fatal("the new holder opened the seat's inbox ahead of the answer it owes")
	}
	if successor.fireRetries() != 1 {
		t.Fatal("the new holder scheduled no attempt at the answer it inherited")
	}
	calls := successor.resumer.calls()
	if len(calls) != 1 || !strings.Contains(calls[0].Answer, "use main") {
		t.Fatalf("resumes = %+v, want the recorded answer", calls)
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held after the inherited answer resumed")
	}
}
