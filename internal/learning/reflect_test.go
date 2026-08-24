package learning_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// --- fixtures -------------------------------------------------------------

// stubWorker is one learning worker whose every outcome is dialable.
type stubWorker struct {
	name   string
	skip   string
	skipFn func(learning.Turn) string
	out    events.Payload
	err    error
	panics bool

	mu   sync.Mutex
	seen []learning.Turn
}

func (w *stubWorker) Name() string { return w.name }

func (w *stubWorker) Skip(t learning.Turn) string {
	if w.skipFn != nil {
		return w.skipFn(t)
	}
	return w.skip
}

func (w *stubWorker) Reflect(_ context.Context, t learning.Turn) ([]events.Payload, error) {
	w.mu.Lock()
	w.seen = append(w.seen, t)
	w.mu.Unlock()
	if w.panics {
		var boom []int
		_ = boom[3] // a worker's own bug, not a returned error
	}
	if w.out == nil {
		return nil, w.err
	}
	return []events.Payload{w.out}, w.err
}

func (w *stubWorker) turns() []learning.Turn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]learning.Turn(nil), w.seen...)
}

func (w *stubWorker) ran() int { return len(w.turns()) }

// recordingPub is a publisher that can also fail or blow up.
type recordingPub struct {
	mu     sync.Mutex
	sent   []*events.Event
	topics []string
	err    error
	panics bool
}

func (p *recordingPub) Publish(_ context.Context, topic string, ev *events.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.panics {
		panic("the broker's connection is closed")
	}
	p.sent = append(p.sent, ev)
	p.topics = append(p.topics, topic)
	return p.err
}

// snapshot copies what has been published so far.
//
// Every reader goes through it and none holds the lock while calling another,
// which is not tidiness: last() used to hold p.mu and then build its failure
// message by calling types(), which takes p.mu again. A sync.Mutex is not
// reentrant, so the deadlock was reachable only on the branch where the event
// was MISSING — the branch a mutation run produces. It cost that run ten
// minutes of test timeout instead of a one-line failure.
func (p *recordingPub) snapshot() []*events.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*events.Event(nil), p.sent...)
}

func (p *recordingPub) types() []string {
	var out []string
	for _, ev := range p.snapshot() {
		out = append(out, ev.Type)
	}
	return out
}

func (p *recordingPub) last(t *testing.T, kind string) *events.Event {
	t.Helper()
	sent := p.snapshot()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].Type == kind {
			return sent[i]
		}
	}
	t.Fatalf("no %s was published; saw %v", kind, p.types())
	return nil
}

func (p *recordingPub) count(kind string) int {
	n := 0
	for _, got := range p.types() {
		if got == kind {
			n++
		}
	}
	return n
}

func devOrg(mutate ...func(*org.Role)) *org.Organization {
	r := &org.Role{Name: "Dev"}
	for _, fn := range mutate {
		fn(r)
	}
	return &org.Organization{Name: "Acme", Roles: []*org.Role{r}}
}

// settledTurn is a done turn that engaged with its trigger.
func settledTurn() types.TurnCompleted {
	return types.TurnCompleted{
		Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev", TurnID: "t1",
		ToolSequence: []string{"search"}, ReviewOutcome: "done",
		PlanDecision: types.PlanDecisionPlan,
	}
}

func reflector(t *testing.T, o *org.Organization, pub queue.Publisher, workers ...learning.Worker) *learning.Reflector {
	t.Helper()
	r, err := learning.NewReflector(o, pub, workers)
	if err != nil {
		t.Fatalf("NewReflector: %v", err)
	}
	return r
}

func reflectOnce(r *learning.Reflector, tc types.TurnCompleted) learning.Reflection {
	return r.Reflect(context.Background(), tc, events.TraceContext{})
}

// --- delivery -------------------------------------------------------------

func TestACompletedTurnReachesTheWorkersThroughTheQueue(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	w := &stubWorker{name: "w"}
	if err := reflector(t, devOrg(), q, w).Start(ctx, q); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ev := events.New(settledTurn(), events.NewTrace())
	if err := q.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if w.ran() != 1 {
		t.Fatalf("worker ran %d times; the dispatcher is attached to the wrong subject", w.ran())
	}
	got := w.turns()[0]
	if got.Event.TurnID != "t1" || got.Role == nil || got.Role.Name != "Dev" {
		t.Errorf("turn = %+v, want the seat resolved from the org", got)
	}
	if got.Trace.TraceID != ev.TraceID {
		t.Errorf("trace = %q, want the turn's own %q", got.Trace.TraceID, ev.TraceID)
	}

	// Counterfactual: a different event type on the same subject space is
	// not a completed turn and must reach nobody.
	other := events.New(types.EpisodeWritten{TurnID: "t2"}, events.NewTrace())
	if err := q.Publish(ctx, topics.Event(other.Type), other); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if w.ran() != 1 {
		t.Errorf("worker ran %d times; it is consuming somebody else's subject", w.ran())
	}
}

func TestAnUnreadableDeliveryIsAckedRatherThanRedelivered(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)
	for _, tc := range []struct {
		name string
		ev   *events.Event
	}{
		{"nothing at all", nil},
		{"an envelope with no body", &events.Event{Type: "turn_completed"}},
		{"another type entirely", events.New(types.EpisodeWritten{}, events.TraceContext{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := r.Handle(context.Background(), tc.ev)
			if res.Outcome != queue.OutcomeAck {
				t.Errorf("outcome = %v, want an ack: redelivering it produces the same non-answer",
					res.Outcome)
			}
		})
	}
	if w.ran() != 0 {
		t.Errorf("worker ran %d times on events it cannot read", w.ran())
	}
}

// Reflection is work about a turn that is already over. A nak would buy a
// second auxiliary call for the same conclusion and, on a turn that reliably
// fails, a dead letter nobody is waiting on.
func TestAFailingPassStillAcks(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w", err: errors.New("the vendor is down")}
	r := reflector(t, devOrg(), &recordingPub{err: errors.New("broker refused")}, w)
	ev := events.New(settledTurn(), events.TraceContext{})
	if res := r.Handle(context.Background(), ev); res.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack", res.Outcome)
	}
	if w.ran() != 1 {
		t.Errorf("worker ran %d times", w.ran())
	}
}

// --- what reflection swallows ---------------------------------------------

func TestAWorkerFailureIsSwallowedAndNamed(t *testing.T) {
	t.Parallel()
	boom := errors.New("aux model refused")
	first := &stubWorker{name: "first", err: boom}
	second := &stubWorker{name: "second", out: types.ReflectionCompleted{TurnID: "t1"}}
	pub := &recordingPub{}
	out := reflectOnce(reflector(t, devOrg(), pub, first, second), settledTurn())

	if !errors.Is(out.Failed["first"], boom) {
		t.Errorf("failed = %v, want the worker's own error kept", out.Failed)
	}
	if second.ran() != 1 {
		t.Error("one worker's failure cost the next one its pass")
	}
	if len(out.Ran) != 2 {
		t.Errorf("ran = %v, want both — a worker that failed still spent the turn's budget", out.Ran)
	}
}

func TestAPanickingWorkerCostsThePassNeitherItsPeersNorItsSentinel(t *testing.T) {
	t.Parallel()
	panicky := &stubWorker{name: "panicky", panics: true}
	after := &stubWorker{name: "after"}
	pub := &recordingPub{}

	out := reflectOnce(reflector(t, devOrg(), pub, panicky, after), settledTurn())

	if out.Failed["panicky"] == nil {
		t.Fatal("a panicking worker was reported as having succeeded")
	}
	if !strings.Contains(out.Failed["panicky"].Error(), "panicked") {
		t.Errorf("error = %v, want it to say what happened", out.Failed["panicky"])
	}
	if after.ran() != 1 {
		t.Error("the worker after the panicking one never ran")
	}
	// The sentinel is what flips the seat back to idle. Losing it leaves
	// the seat rendering as working for ever — the visible half of a bug
	// whose invisible half is simply no learning.
	if pub.count("reflection_completed") != 1 {
		t.Errorf("published %v, want the trailing sentinel", pub.types())
	}
}

func TestAPanickingPublisherDoesNotEscapeTheHandler(t *testing.T) {
	t.Parallel()
	r := reflector(t, devOrg(), &recordingPub{panics: true}, &stubWorker{name: "w"})
	res := r.Handle(context.Background(), events.New(settledTurn(), events.TraceContext{}))
	if res.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack; the panic must not reach the consumer goroutine", res.Outcome)
	}
}

func TestAPublishFailureDoesNotUndoTheLearning(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w", out: types.PersistDeciderCompleted{TurnID: "t1"}}
	out := reflectOnce(reflector(t, devOrg(), &recordingPub{err: errors.New("broker refused")}, w), settledTurn())
	if len(out.Ran) != 1 || len(out.Failed) != 0 {
		t.Errorf("pass = %+v; a lost announcement is not a failed worker", out)
	}
}

// --- the gates ------------------------------------------------------------

func TestOnlySettledTurnsReachAWorkerThatAsksForThem(t *testing.T) {
	t.Parallel()
	// The rule is the dispatcher's vocabulary and the worker's policy: the
	// persist decider must not learn from a turn the engine will reattempt,
	// while observing who you talked to does not depend on what the agent
	// decided to do next. Turn.Settled is the shared definition.
	settledOnly := func(x learning.Turn) string {
		if x.Settled() {
			return ""
		}
		return "non_terminal"
	}
	for _, tc := range []struct {
		outcome     string
		settledOnly bool
		wantRun     bool
	}{
		{"done", true, true},
		{"failed", true, true},
		{"self_iterate", true, false},
		{"self_iterate", false, true},
		{"", true, false},
		// An outcome nothing recognises is not settled either: only done
		// and failed mean the turn ran its course.
		{"escalated", true, false},
	} {
		t.Run(fmt.Sprintf("%s/settled-only=%v", tc.outcome, tc.settledOnly), func(t *testing.T) {
			t.Parallel()
			w := &stubWorker{name: "w"}
			if tc.settledOnly {
				w.skipFn = settledOnly
			}
			turn := settledTurn()
			turn.ReviewOutcome = tc.outcome
			out := reflectOnce(reflector(t, devOrg(), &recordingPub{}, w), turn)
			if got := w.ran() == 1; got != tc.wantRun {
				t.Errorf("ran = %v, want %v (skips: %v)", got, tc.wantRun, out.Skipped)
			}
			if !tc.wantRun && out.Skipped["w"] != "non_terminal" {
				t.Errorf("skip reason = %q, want non_terminal", out.Skipped["w"])
			}
		})
	}
}

func TestAWorkerThatSkipsIsNotCountedAsHavingRun(t *testing.T) {
	t.Parallel()
	skipped := &stubWorker{name: "skipped", skip: "self_persisted"}
	pub := &recordingPub{}
	out := reflectOnce(reflector(t, devOrg(), pub, skipped), settledTurn())

	if skipped.ran() != 0 {
		t.Error("a worker that declared a skip was dispatched anyway")
	}
	if len(out.Ran) != 0 || out.Skipped["skipped"] != "self_persisted" {
		t.Errorf("pass = %+v", out)
	}
	// The count is what a dashboard plots as "did this company learn
	// anything". A pass whose every worker declined has to read as zero.
	ev := pub.last(t, "reflection_completed")
	if got := ev.Data.(*types.ReflectionCompleted).WorkersRun; got != 0 {
		t.Errorf("workers_run = %d, want 0", got)
	}
}

func TestASeatWhoseRoleIsGoneIsSkipped(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	pub := &recordingPub{}
	turn := settledTurn()
	turn.RoleName = "Removed"
	out := reflectOnce(reflector(t, devOrg(), pub, w), turn)
	if out.Skip != learning.SkipNoRole || w.ran() != 0 {
		t.Errorf("pass = %+v, worker ran %d times", out, w.ran())
	}
	if len(pub.types()) != 0 {
		t.Errorf("published %v for a seat that does not exist", pub.types())
	}
	// Counterfactual: the same turn under its real role runs.
	if out := reflectOnce(reflector(t, devOrg(), pub, w), settledTurn()); out.Skip != "" {
		t.Errorf("the control pass skipped: %+v", out)
	}
}

func TestLearningIsOffOnlyWhenTheOperatorSaidSo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		toggle  org.Toggle
		wantRun bool
	}{
		{"silent", org.Toggle{}, true},
		{"explicitly on", org.On(), true},
		{"explicitly off", org.Off(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &stubWorker{name: "w"}
			o := devOrg(func(r *org.Role) { r.LearningEnabled = tc.toggle })
			out := reflectOnce(reflector(t, o, &recordingPub{}, w), settledTurn())
			if got := w.ran() == 1; got != tc.wantRun {
				t.Errorf("ran = %v, want %v (%+v)", got, tc.wantRun, out)
			}
			if !tc.wantRun && out.Skip != learning.SkipRoleDisabled {
				t.Errorf("skip = %q, want %q", out.Skip, learning.SkipRoleDisabled)
			}
		})
	}
}

func TestATurnThatEngagedWithNothingIsSkippedButStillAnnounced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mutate  func(*types.TurnCompleted)
		wantRun bool
	}{
		// The planner recognised the trigger was for somebody else.
		{"the planner opted out", func(x *types.TurnCompleted) {
			x.PlanDecision = types.PlanDecisionSkip
		}, false},
		// It MEANT to opt out, never called submit_plan, and Execute ran
		// with the full registry and called nothing.
		{"it called nothing and called that done", func(x *types.TurnCompleted) {
			x.ToolSequence = nil
		}, false},
		{"it opted out having called tools anyway", func(x *types.TurnCompleted) {
			x.PlanDecision = types.PlanDecisionSkip
			x.ToolSequence = []string{"slack_post"}
		}, false},
		// A failed turn that called nothing failed AT something, and that
		// is worth learning from.
		{"it called nothing and failed", func(x *types.TurnCompleted) {
			x.ToolSequence = nil
			x.ReviewOutcome = "failed"
		}, true},
		{"no plan artifact at all, but it acted", func(x *types.TurnCompleted) {
			x.PlanDecision = ""
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := &stubWorker{name: "w"}
			pub := &recordingPub{}
			turn := settledTurn()
			tc.mutate(&turn)
			out := reflectOnce(reflector(t, devOrg(), pub, w), turn)

			if got := w.ran() == 1; got != tc.wantRun {
				t.Fatalf("ran = %v, want %v (%+v)", got, tc.wantRun, out)
			}
			if tc.wantRun {
				return
			}
			if out.Skip != learning.SkipNoEngagement {
				t.Errorf("skip = %q, want %q", out.Skip, learning.SkipNoEngagement)
			}
			// A turn the dispatcher DECIDED not to learn from and a turn
			// reflection never reached look identical otherwise, and only
			// the second is a bug.
			ev := pub.last(t, "reflection_completed")
			if got := ev.Data.(*types.ReflectionCompleted).WorkersRun; got != 0 {
				t.Errorf("workers_run = %d, want 0", got)
			}
		})
	}
}

func TestARedeliveredTurnIsReflectedOnce(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)
	first := reflectOnce(r, settledTurn())
	second := reflectOnce(r, settledTurn())

	if first.Skip != "" || second.Skip != learning.SkipDuplicate {
		t.Errorf("first = %+v, second = %+v", first, second)
	}
	if w.ran() != 1 {
		t.Errorf("worker ran %d times; each pass is a fresh aux call that can write a second row", w.ran())
	}
	// Counterfactual: a different turn is not a duplicate.
	other := settledTurn()
	other.TurnID = "t2"
	if out := reflectOnce(r, other); out.Skip != "" {
		t.Errorf("a distinct turn was refused as a duplicate: %+v", out)
	}
}

// The guard is BOUNDED, so it cannot be a memory leak in a process that runs
// for months. Past the bound a redelivery is reflected again — bounded
// duplication, which is what the engine promises instead of exactly-once.
func TestTheRedeliveryGuardIsBounded(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)
	first := settledTurn()
	reflectOnce(r, first)
	for i := range 1024 {
		turn := settledTurn()
		turn.TurnID = fmt.Sprintf("filler-%d", i)
		reflectOnce(r, turn)
	}
	if out := reflectOnce(r, first); out.Skip != "" {
		t.Errorf("skip = %q, want the oldest id to have aged out of a bounded guard", out.Skip)
	}
}

func TestNothingWiredIsAFastPath(t *testing.T) {
	t.Parallel()
	pub := &recordingPub{}
	out := reflectOnce(reflector(t, devOrg(), pub), settledTurn())
	if out.Skip != learning.SkipNoWorkers {
		t.Errorf("skip = %q, want %q", out.Skip, learning.SkipNoWorkers)
	}
	if len(pub.types()) != 0 {
		t.Errorf("published %v with nothing wired to reflect", pub.types())
	}
}

// --- what the pass announces ----------------------------------------------

func TestLifecycleEventsHangOffTheTurnThatCausedThem(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w", out: types.PersistDeciderCompleted{TurnID: "t1", Classification: types.PersistLong}}
	pub := &recordingPub{}
	r := reflector(t, devOrg(), pub, w)

	ev := events.New(settledTurn(), events.NewTrace())
	ev.ParentSpanID = "aaaaaaaaaaaaaaaa"
	r.Handle(context.Background(), ev)

	for _, kind := range []string{"persist_decider_completed", "reflection_completed"} {
		got := pub.last(t, kind)
		if got.TraceID != ev.TraceID || got.SpanID != ev.SpanID || got.ParentSpanID != ev.ParentSpanID {
			t.Errorf("%s carries trace %q/%q/%q, want the turn's %q/%q/%q — otherwise it renders "+
				"as background work with no cause", kind, got.TraceID, got.SpanID, got.ParentSpanID,
				ev.TraceID, ev.SpanID, ev.ParentSpanID)
		}
		if got.Source != "Dev" {
			t.Errorf("%s source = %q, want the seat", kind, got.Source)
		}
	}
	if pub.topics[0] != topics.Event("persist_decider_completed") {
		t.Errorf("published to %q", pub.topics[0])
	}
	if got := pub.last(t, "reflection_completed").Data.(*types.ReflectionCompleted); got.WorkersRun != 1 ||
		got.Agent != "agent-uuid" || got.AgentHandle != "dev" || got.ReviewOutcome != "done" {
		t.Errorf("sentinel = %+v", got)
	}
}

func TestAWorkerWithNothingToAnnounceStillCounts(t *testing.T) {
	t.Parallel()
	pub := &recordingPub{}
	out := reflectOnce(reflector(t, devOrg(), pub, &stubWorker{name: "quiet"}), settledTurn())
	if len(out.Ran) != 1 {
		t.Errorf("ran = %v", out.Ran)
	}
	if pub.count("reflection_completed") != 1 || len(pub.types()) != 1 {
		t.Errorf("published %v, want only the sentinel", pub.types())
	}
}

// --- wiring ---------------------------------------------------------------

func TestADispatcherRefusesWiringItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	if _, err := learning.NewReflector(nil, &recordingPub{}, nil); err == nil {
		t.Error("a dispatcher with no org was accepted; every turn would skip on an unresolvable seat")
	}
	if _, err := learning.NewReflector(devOrg(), nil, nil); err == nil {
		t.Error("a dispatcher with no publisher was accepted")
	}
	if _, err := learning.NewReflector(devOrg(), &recordingPub{}, []learning.Worker{nil}); err == nil {
		t.Error("a nil worker was accepted; it panics on the first completed turn")
	}
	if _, err := learning.NewReflector(devOrg(), &recordingPub{}, nil); err != nil {
		t.Errorf("a dispatcher with no workers yet was refused: %v", err)
	}
	// A pass reports its skips and failures BY WORKER NAME, so two workers
	// sharing one would each erase the other's entry and an operator would
	// read one worker's failure as the other's.
	twins := []learning.Worker{&stubWorker{name: "same"}, &stubWorker{name: "same"}}
	if _, err := learning.NewReflector(devOrg(), &recordingPub{}, twins); err == nil {
		t.Error("two workers under one name were accepted")
	}
}

// The dispatcher is one object serving a whole node, and the queue may hand
// it several completed turns at once.
func TestConcurrentDeliveriesAreSafe(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn := settledTurn()
			// Half the goroutines race on ONE id, so the dedup guard is
			// exercised under contention as well as the dispatch.
			if i%2 == 0 {
				turn.TurnID = fmt.Sprintf("t%d", i)
			}
			r.Handle(context.Background(), events.New(turn, events.TraceContext{}))
		}()
	}
	wg.Wait()
	if got := w.ran(); got != 9 {
		t.Errorf("worker ran %d times, want 8 distinct turns plus one of the racing id", got)
	}
}

// --- the decider as a worker ----------------------------------------------

// The one wiring that matters today: the dispatcher's gates in front of the
// real decider, with a scripted model behind it.
func TestTheDeciderRunsUnderTheDispatcher(t *testing.T) {
	t.Parallel()
	store := &fakeDiary{}
	p := says(`{"kind":"LONG","content":"Sam reviews on Mondays."}`)
	d := decider(t, p, store)
	pub := &recordingPub{}
	r := reflector(t, devOrg(), pub, d)

	out := reflectOnce(r, settledTurn())
	if len(out.Ran) != 1 || len(store.written()) != 1 {
		t.Fatalf("pass = %+v, wrote %d rows", out, len(store.written()))
	}
	ev := pub.last(t, "persist_decider_completed").Data.(*types.PersistDeciderCompleted)
	if !ev.Persisted || ev.Classification != types.PersistLong || ev.Scope != types.MemoryScopeAgent {
		t.Errorf("event = %+v", ev)
	}

	// A turn that already wrote its own memory must not be classified
	// again: the aux call is the expensive half and the dedup block only
	// suppresses the duplicate if the model recognises its own paraphrase.
	self := settledTurn()
	self.TurnID = "t2"
	self.PlanToolSequence = []string{learning.ReflectTool}
	out = reflectOnce(r, self)
	if out.Skipped["persist_decider"] != "self_persisted" {
		t.Errorf("pass = %+v, want the decider skipped", out)
	}
	if p.count() != 1 {
		t.Errorf("the model was called %d times; the second turn had already persisted", p.count())
	}
	if len(store.written()) != 1 {
		t.Errorf("%d rows written, want the self-persisted turn to have added none", len(store.written()))
	}

	// Counterfactual: a turn naming some OTHER tool is not self-persisted.
	third := settledTurn()
	third.TurnID = "t3"
	third.PlanToolSequence = []string{"query_episodes"}
	if out := reflectOnce(r, third); len(out.Ran) != 1 {
		t.Errorf("pass = %+v, want the decider to run", out)
	}
}

func TestAMidTurnStateNeverReachesTheDecider(t *testing.T) {
	t.Parallel()
	store := &fakeDiary{}
	p := says(`{"kind":"LONG","content":"a durable fact"}`)
	r := reflector(t, devOrg(), &recordingPub{}, decider(t, p, store))

	turn := settledTurn()
	turn.ReviewOutcome = "self_iterate"
	out := reflectOnce(r, turn)

	if out.Skipped["persist_decider"] != "non_terminal" {
		t.Errorf("pass = %+v, want the decider skipped on a state the engine will reattempt", out)
	}
	if p.count() != 0 || len(store.written()) != 0 {
		t.Errorf("model called %d times, %d rows written from an incomplete turn",
			p.count(), len(store.written()))
	}
}

// --- surviving an apply ---------------------------------------------------
//
// The dispatcher is ONE PER PROCESS and an apply swaps what runs behind it.
// The alternative — rebuilding it per epoch — empties the redelivery ring,
// so a redelivery landing either side of a config change is classified
// twice: two auxiliary calls, two differently-worded rows for one fact.

func TestReconfigureSwapsTheWorkerSet(t *testing.T) {
	t.Parallel()
	before, after := &stubWorker{name: "before"}, &stubWorker{name: "after"}
	r := reflector(t, devOrg(), &recordingPub{}, before)

	if err := r.Reconfigure(devOrg(), []learning.Worker{after}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	res := reflectOnce(r, settledTurn())
	if len(res.Ran) != 1 || res.Ran[0] != "after" {
		t.Fatalf("ran %v, want only the new epoch's worker", res.Ran)
	}
	if len(before.turns()) != 0 {
		t.Error("the previous epoch's worker still ran")
	}
}

// THE RING SURVIVES THE SWAP, which is the whole reason the dispatcher
// outlives an epoch: a redelivery arriving just after an apply must not be
// reflected on a second time.
func TestAReconfigureDoesNotForgetWhatWasAlreadyReflected(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)
	if res := reflectOnce(r, settledTurn()); res.Skip != "" {
		t.Fatalf("the first pass was skipped: %s", res.Skip)
	}

	if err := r.Reconfigure(devOrg(), []learning.Worker{w}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if res := reflectOnce(r, settledTurn()); res.Skip != learning.SkipDuplicate {
		t.Fatalf("skip = %q after an apply, want the redelivery still "+
			"recognised as a duplicate", res.Skip)
	}
}

// A BAD WORKER SET LEAVES THE PREVIOUS ONE SERVING. Reflecting against a
// stale epoch is a far smaller wrong than not reflecting at all, and the
// caller cannot fix a duplicate worker name by trying again.
func TestARefusedReconfigureKeepsThePreviousEpochServing(t *testing.T) {
	t.Parallel()
	good := &stubWorker{name: "good"}
	r := reflector(t, devOrg(), &recordingPub{}, good)

	for _, bad := range [][]learning.Worker{
		{&stubWorker{name: "dup"}, &stubWorker{name: "dup"}},
		{nil},
	} {
		if err := r.Reconfigure(devOrg(), bad); err == nil {
			t.Fatalf("Reconfigure accepted %d bad workers", len(bad))
		}
	}
	if err := r.Reconfigure(nil, []learning.Worker{good}); err == nil {
		t.Fatal("Reconfigure accepted a nil org")
	}
	if res := reflectOnce(r, settledTurn()); len(res.Ran) != 1 || res.Ran[0] != "good" {
		t.Fatalf("ran %v, want the previous epoch's worker still serving", res.Ran)
	}
}

// AN APPLY THAT REMOVES EVERY WORKER really does stop the passes — the
// company turned learning off, and the fast path is what makes that free.
func TestReconfiguringToNoWorkersStopsThePasses(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)
	if err := r.Reconfigure(devOrg(), nil); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if res := reflectOnce(r, settledTurn()); res.Skip != learning.SkipNoWorkers {
		t.Fatalf("skip = %q, want no_workers", res.Skip)
	}
}

// THE NEW EPOCH'S ORG DECIDES. A seat the revision removed must stop being
// learned about, and one it added must start.
func TestReconfigureSwapsTheOrgTheSeatIsResolvedAgainst(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)

	renamed := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Engineer"}}}
	if err := r.Reconfigure(renamed, []learning.Worker{w}); err != nil {
		t.Fatalf("Reconfigure: %v", err)
	}
	if res := reflectOnce(r, settledTurn()); res.Skip != learning.SkipNoRole {
		t.Fatalf("skip = %q, want the removed role to stop being reflected on",
			res.Skip)
	}

	moved := settledTurn()
	moved.RoleName, moved.TurnID = "Engineer", "t2"
	if res := reflectOnce(r, moved); res.Skip != "" {
		t.Fatalf("skip = %q, want the new epoch's role to be reflected on",
			res.Skip)
	}
}

// THE REDELIVERY RING IS BOUNDED, and that bound is the point: the guard is
// per-process and lives for months, so a set that only ever grew would be a
// slow leak keyed on every turn the company has ever taken. Past the bound,
// a redelivery of a turn this process reflected on long ago is no longer a
// case worth spending memory against — it is far outside any backend's
// redelivery window.
func TestTheRedeliveryGuardEvictsRatherThanGrowing(t *testing.T) {
	t.Parallel()
	w := &stubWorker{name: "w"}
	r := reflector(t, devOrg(), &recordingPub{}, w)

	first := settledTurn()
	if res := reflectOnce(r, first); res.Skip != "" {
		t.Fatalf("the first pass was skipped: %s", res.Skip)
	}
	// One more than the ring holds, so the first id is evicted.
	for i := range learning.ReflectSeen {
		tc := settledTurn()
		tc.TurnID = fmt.Sprintf("filler-%d", i)
		reflectOnce(r, tc)
	}
	if res := reflectOnce(r, first); res.Skip == learning.SkipDuplicate {
		t.Fatal("an id far past the bound is still remembered, so the guard " +
			"grows without limit")
	}

	// And the RECENT ones are still remembered, which is what the guard is
	// actually for — an eviction policy that forgot everything would pass
	// the check above and dedupe nothing.
	recent := settledTurn()
	recent.TurnID = fmt.Sprintf("filler-%d", learning.ReflectSeen-1)
	if res := reflectOnce(r, recent); res.Skip != learning.SkipDuplicate {
		t.Fatalf("skip = %q for the most recent turn, want it deduped", res.Skip)
	}
}
