package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// THE R1/R2 RACE, end to end through a seat's inbox, on BOTH backends.
//
//  1. An executor parks with a question in a chat thread.
//  2. The first reply, R1, arrives; the run's resume fails on something
//     transient.
//  3. The person's next message, R2, arrives before anything retries.
//  4. The run asks a SECOND question later, and a copy of R1 arrives after it.
//
// The run must be resumed with R1, R2 must not answer the question (it is
// worked as an ordinary turn, AFTER the resume), and R1 must not answer the
// second question.
//
// It went the other way because R1's delivery was handed back with a Nak,
// which on JetStream returns it after its backoff BEHIND R2 — so R2 reached
// the still-waiting run first and was spliced in as its answer, and R1 came
// round to answer whatever was asked next — while the in-memory twin replayed
// a Nak at the head, so every engine test certified the order production never
// gave. The same scenario runs here on the twin and on an embedded JetStream
// broker at the conformance suite's timings, and both must tell the same
// story.
func TestTheFirstReplyAnswersTheQuestionOnEveryBackend(t *testing.T) {
	// NOT PARALLEL, and for the broker's sake rather than any shared state:
	// at the conformance suite's two-second ack wait, a handler a contended
	// runner slows past it is redelivered, and a redelivery reorders the
	// very replies this case asserts the order of. Run in the parallel
	// phase it would be measuring the runner's load.
	for name, open := range map[string]func(t *testing.T) queue.EventQueue{
		"memory_twin": func(t *testing.T) queue.EventQueue {
			return memory.New()
		},
		"embedded_jetstream": func(t *testing.T) queue.EventQueue {
			// The conformance harness's timings: a production backoff
			// would make the retry R1 used to get a second long, and
			// the order under test is the same at any scale.
			srv, err := jetstream.StartServer(t.Context(), jetstream.Config{
				AckWait: 2 * time.Second, FetchWait: 25 * time.Millisecond,
				NakDelay: 25 * time.Millisecond, NakCeiling: 50 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("StartServer: %v", err)
			}
			t.Cleanup(srv.Shutdown)
			q, err := srv.Client(t.Context())
			if err != nil {
				t.Fatalf("Client: %v", err)
			}
			return q
		},
	} {
		t.Run(name, func(t *testing.T) {
			runAnswerOrder(t, open(t))
		})
	}
}

// answerResumer resumes the parked run as a turn would: it fails the first
// time, and on the next it relaunches its executor and the new job parks on a
// second question.
type answerResumer struct {
	store *sandbox.CoordStore

	mu      sync.Mutex
	calls   int
	answers []string
}

func (r *answerResumer) Resume(ctx context.Context, req sandbox.ResumeRequest) error {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	if !first {
		// A person's answer carries the run on: it rides the request's
		// Asked, never its Answer, which is a collected run's own report.
		var answer string
		if req.Asked != nil {
			answer = req.Asked.Answer
		}
		r.answers = append(r.answers, answer)
	}
	r.mu.Unlock()
	if first {
		return fmt.Errorf("%w: the runner is still being built on this node", sandbox.ErrResumeUnavailable)
	}
	// The resumed executor launches the next job, and it asks again.
	if _, err := r.store.BeginLaunch(ctx, req.Run, sandbox.Fence{}); err != nil {
		return err
	}
	if _, err := r.store.MarkSuspended(ctx, req.Run.TurnID,
		sandbox.Suspension{State: json.RawMessage(`{"messages":[]}`)}); err != nil {
		return err
	}
	if err := sandboxtest.RecordAsk(ctx, r.store, req.Run.TurnID,
		sandbox.Ask{Question: "which test suite?", To: sandbox.AskToRequester}); err != nil {
		return err
	}
	return r.store.MarkAwaiting(ctx, req.Run.TurnID, sandbox.Clarification{AskedAt: time.Now().UTC()})
}

func (r *answerResumer) resumedWith() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.answers...)
}

// inboxHold is the engine's sandbox seat hold, on the queue under test.
type inboxHold struct{ q queue.EventQueue }

func (h inboxHold) Hold(ctx context.Context, handle string) error {
	return h.q.PauseTopic(ctx, fixtureInbox(handle), fixtureInboxGroup(handle),
		string(inbox.HoldSandbox))
}

func (h inboxHold) Release(ctx context.Context, handle string) error {
	return h.q.ResumeTopic(ctx, fixtureInbox(handle), fixtureInboxGroup(handle),
		string(inbox.HoldSandbox))
}

// fixtureOrg is the company the hand-built rigs in these cases stand in for.
const fixtureOrg = "Acme"

// fixtureSeat is the id a handle derives in [fixtureOrg]: what a seat's
// mailbox is named by, what a run's row carries as its AgentID, and what the
// coordinator hands a reply back to. No engine runs a company in these rigs,
// so nothing else would name it — and a mailbox spelled from the handle is one
// the coordinator never publishes to, so every assertion over it would pass by
// being empty.
func fixtureSeat(handle string) uuid.UUID {
	id, _ := org.DeriveAgentID(fixtureOrg, handle)
	return id
}

// fixtureInbox and fixtureInboxGroup are a seat's mailbox in [fixtureOrg].
func fixtureInbox(handle string) string { return topics.AgentInbox(fixtureSeat(handle)) }

func fixtureInboxGroup(handle string) string {
	return topics.AgentInboxGroup(fixtureSeat(handle))
}

// handCranked is time.AfterFunc driven by the test.
type handCranked struct {
	mu  sync.Mutex
	due []func()
	// at is each due timer's delay, beside it in due, for a case that fires
	// only what was asked for at once ([handCranked.fireNow]).
	at []time.Duration
}

func (h *handCranked) after(delay time.Duration, f func()) func() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	fired := false
	h.at = append(h.at, delay)
	h.due = append(h.due, func() {
		if !fired {
			fired = true
			f()
		}
	})
	return func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		if fired {
			return false
		}
		fired = true
		return true
	}
}

func (h *handCranked) fire() int {
	h.mu.Lock()
	due := h.due
	h.due, h.at = nil, nil
	h.mu.Unlock()
	for _, f := range due {
		f()
	}
	return len(due)
}

// fireNow fires only the timers armed with no delay, the ones a signal armed,
// and leaves every timer waiting on the clock, a backstop's included.
func (h *handCranked) fireNow() int {
	h.mu.Lock()
	var now []func()
	var due []func()
	var at []time.Duration
	for i, f := range h.due {
		if h.at[i] == 0 {
			now = append(now, f)
			continue
		}
		due, at = append(due, f), append(at, h.at[i])
	}
	h.due, h.at = due, at
	h.mu.Unlock()
	for _, f := range now {
		f()
	}
	return len(now)
}

func (h *handCranked) pending() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.due)
}

// turns records the ordinary turns the seat ran, safely across the consumer
// goroutine a broker-backed queue dispatches on.
type turns struct {
	mu   sync.Mutex
	asks []string
}

func (s *turns) run(_ context.Context, req engine.Request) (turn.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asks = append(s.asks, engine.DescribeTrigger(req.Events, ""))
	return turn.Result{Decision: phase.Done}, nil
}

func (s *turns) ran() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asks...)
}

// threadReply is a chat message in the thread the run asked its question in.
func threadReply(body string) *events.Event {
	e := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: "ana", Body: body,
	}, events.TraceContext{})
	e.Source = "notify.slack"
	notifyStamp(e, "slack:C1:1.0", "slack:C1:1.0")
	return e
}

// eventually polls cond until it holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle gives a broker-backed consumer time to have delivered anything it
// was going to — several fetch waits — for the assertions that something did
// NOT happen.
func settle() { time.Sleep(250 * time.Millisecond) }

func runAnswerOrder(t *testing.T, q queue.EventQueue) {
	ctx := t.Context()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })

	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	resumer := &answerResumer{store: store}
	clock := &handCranked{}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers:  fleetAnswers(),
		Audience: noAudience{}, Queue: q, Pending: store, Manager: manager,
		Resume: resumer, Hold: inboxHold{q: q}, After: clock.after,
		// THE SEAT HELD UNDER THE LEASE IT IS RECOVERED WITH BELOW, as a
		// seat host's acquisition holds it.
		Lease: func(string) (sandbox.Fence, bool) {
			return sandbox.Fence{Owner: "node-a", Epoch: 1}, true
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)

	// THE PARKED RUN: launched from the thread, its question asked now.
	run := sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: fixtureSeat("swe").String(), Role: "SWE", CodingAgent: "claude-code",
		ConversationKey: "slack:C1:1.0", PartitionKey: "slack:C1:1.0",
	}
	if _, err := store.BeginLaunch(ctx, run, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if _, err := store.MarkSuspended(ctx, "t1",
		sandbox.Suspension{State: json.RawMessage(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	sandboxtest.Ask(t, store, "t1", sandbox.Ask{Question: "which branch?", To: sandbox.AskToRequester})
	if err := store.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if err := coordinator.RecoverSeat(ctx, "swe", "node-a", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}

	ordinary := &turns{}
	d := &engine.Dispatcher{
		Ledgered:    func(kind string) bool { return kind == notificationType },
		Turn:        ordinary.run,
		Completions: ledgerstore.NewMemoryCompletions(),
		Answer:      coordinator.TryResumeFromAnswer,
		Conditions: func(handle string) inbox.Conditions {
			held, awaits := coordinator.SeatRuns(handle)
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				SeatHeldBySandbox: held, SandboxAwaitsAnswer: awaits}
		},
		Park: func(ctx context.Context, handle string, evs []*events.Event) error {
			for _, ev := range evs {
				if err := q.Publish(ctx, fixtureInbox(handle), ev); err != nil {
					return err
				}
			}
			return nil
		},
		NoteDeferred: func(string) {},
	}
	if err := q.Subscribe(ctx, fixtureInbox("swe"), fixtureInboxGroup("swe"),
		func(ctx context.Context, ev *events.Event) queue.Result {
			return d.Dispatch(ctx, "swe", []*events.Event{ev})
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	inboxOf := fixtureInbox("swe")
	get := func() sandbox.PendingRun {
		got, _, err := store.Get(ctx, "t1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return got
	}

	// R1 ARRIVES, and its resume fails.
	r1 := threadReply("use the release branch")
	if err := q.Publish(ctx, inboxOf, r1); err != nil {
		t.Fatalf("Publish R1: %v", err)
	}
	eventually(t, "R1 to be recorded as the answer and its retry scheduled", func() bool {
		return get().Status == sandbox.StatusAnswered && clock.pending() == 1
	})

	// R2 ARRIVES BEFORE ANYTHING RETRIES.
	r2 := threadReply("actually, also update the docs")
	if err := q.Publish(ctx, inboxOf, r2); err != nil {
		t.Fatalf("Publish R2: %v", err)
	}
	settle()
	got := get()
	if got.Status != sandbox.StatusAnswered || got.Answer == nil {
		t.Fatalf("run = %q with answer %+v, want R1 still the recorded answer", got.Status, got.Answer)
	}
	if words, err := fleetAnswers().Open(got); err != nil || !strings.Contains(words.Text, "release branch") {
		t.Fatalf("the recorded answer opens to %+v, %v, want R1", words, err)
	}
	if ran := ordinary.ran(); len(ran) != 0 {
		t.Fatalf("R2 was worked as a turn (%q) while R1's resume was still owed: the seat's "+
			"inbox must hold its later mail behind the answer", ran)
	}

	// THE RETRY RESUMES THE RUN WITH R1, and only then is R2 worked.
	if clock.fire() != 1 {
		t.Fatal("no retry was scheduled for R1's resume")
	}
	resumed := resumer.resumedWith()
	if len(resumed) != 1 || !strings.Contains(resumed[0], "release branch") ||
		strings.Contains(resumed[0], "docs") {
		t.Fatalf("the run was resumed with %q, want R1 and nothing of R2", resumed)
	}
	eventually(t, "R2 to be worked as the ordinary message it is", func() bool {
		return len(ordinary.ran()) == 1
	})
	if ran := ordinary.ran(); !strings.Contains(ran[0], "docs") {
		t.Fatalf("the ordinary turn was asked %q, want R2", ran[0])
	}

	// THE RUN ASKED A SECOND QUESTION. A copy of R1 — a redelivery whose
	// acknowledgement was lost — does not answer it.
	second := get()
	if second.Status != sandbox.StatusAwaiting || second.Question != "which test suite?" {
		t.Fatalf("run = %q on %q, want it parked on its second question", second.Status, second.Question)
	}
	if err := q.Publish(ctx, inboxOf, r1); err != nil {
		t.Fatalf("Publish R1 again: %v", err)
	}
	settle()
	if got := get(); got.Status != sandbox.StatusAwaiting || got.Answer != nil {
		t.Fatalf("run = %q with answer %+v, want the second question still open: R1 "+
			"was written before it was asked", got.Status, got.Answer)
	}
	if ran := ordinary.ran(); len(ran) != 1 {
		t.Fatalf("the copy of R1 was worked as a turn as well (%q): it is spent", ran)
	}
	// AND BY THE ANCHOR ALONE, without the ledger that dropped the copy
	// above: R1 was posted before the second question was asked.
	if d, err := coordinator.TryResumeFromAnswer(ctx, "swe", sandbox.Reply{
		Conv: sandbox.ConversationRef{Identity: "slack:C1:1.0", Partition: "slack:C1:1.0"},
		Text: "use the release branch", Events: []*events.Event{r1},
	}); err != nil || d != sandbox.AnswerNotMine {
		t.Fatalf("R1 offered to the second question = %q, %v, want %q", d, err, sandbox.AnswerNotMine)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("the test context ended early")
	}
}
