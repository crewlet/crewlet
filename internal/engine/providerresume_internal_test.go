package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RESUME NEVER RUNS INTO A LIMIT ITS MODELS ALREADY KNOW, and one that meets
// a limit it did not know waits for it: neither is a failure.
//
// What a running engine did: a launch's completion reached a seat whose
// models were all at their usage limit, the resume ran a turn into the limit,
// the turn was reverted, and the control topic NAKed the completion, which the
// broker redelivered into the same limit 25 times in about ten minutes.

// spentEntry is a model whose harness already reported its usage limit, as
// harness's entry knows it ([chain.Unready]), until the case lifts it. calls
// counts what reached it, which a limit its entry knows should keep out.
type spentEntry struct {
	limited atomic.Bool
	retry   time.Time
	calls   atomic.Int32
}

func (*spentEntry) Model() string { return "opus" }

func (m *spentEntry) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	m.calls.Add(1)
	if err := m.Unready(ctx); err != nil {
		return nil, err
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

func (m *spentEntry) Unready(context.Context) error {
	if !m.limited.Load() {
		return nil
	}
	return &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: m.retry,
		Err: errors.New("You've hit your 5-hour limit")}
}

// THE GATE REFUSES AN ANSWER WHOSE MODELS ARE AT A LIMIT THEY KNOW, before the
// claim and with the instant they are tried again, and the control topic
// acknowledges that refusal. A COMPLETION it admits: its job is over, so it is
// collected first, and its resume is refused the same way once it is
// (collectedRefusal), which parks its seat on the limit. A resume that met the
// limit mid-turn is refused the same way, and a spent window as its own wait.
//
// Mutation: drop the seat's conditions from admitResume and the limited answer
// is admitted; ask a completion's gate the limit again (seatCannotRun) and it
// is refused before its claim; drop the park from collectedRefusal and the
// seat is not parked; drop the mapping from the resume's failure and a limit
// met mid-turn is a plain error, which the control topic NAKs.
func TestAResumeAtItsModelsLimitIsAWaitNotAFailure(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	e := r.start(t)
	e.providerParks.after = (&fakeAlarms{}).after
	spent := harnessProvider(t, chainMember{key: "default", agent: "claude-code", model: "opus", spent: true})
	retry, _, limited := llm.LimitedUntil(spent.SittingOut())
	if !limited {
		t.Fatalf("the premise: the spent entry sits out nothing (%v)", spent.SittingOut())
	}
	withLaunchModels(t, e, spent)
	turns := e.node.InFlightTurns()

	_, release, err := e.admitResume(t.Context(), "swe", "", sandbox.ResumeAnswer)
	if err == nil {
		// Admitted: the slot is given back before the case fails, so the
		// engine's stop does not wait on it.
		release()
	}
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition != waitProvider || !refused.Until.Equal(retry) {
		t.Fatalf("admitResume of an answer on a limited seat = %v, want a refusal waiting on "+
			"%q until %v", err, waitProvider, retry)
	}
	if got := e.node.InFlightTurns(); got != turns {
		t.Errorf("a refused resume kept the node's admission: %d turns in flight, want %d", got, turns)
	}
	if got := controlOutcome(t.Context(), err).Outcome; got != queue.OutcomeAck {
		t.Errorf("the control topic hands a limited resume back as %s, want ack", got)
	}

	// A COMPLETION IS ADMITTED, to be collected before its limit is asked.
	_, release, err = e.admitResume(t.Context(), "swe", "", sandbox.ResumeCompletion)
	if err != nil {
		t.Fatalf("admitResume of a completion on a limited seat = %v, want it admitted to be "+
			"collected first", err)
	}
	release()

	limitedChain := &chain.Error{Attempted: []string{"default"},
		Failures: []error{spent.SittingOut()}, Err: spent.SittingOut()}
	if refusal, ok := resumeRefusal(limitedChain); !ok ||
		refusal.Condition != waitProvider || !refusal.Until.Equal(retry) {
		t.Errorf("a resume that met the limit = %+v (%v), want it waiting on %q until %v",
			refusal, ok, waitProvider, retry)
	}
	// A SPENT WINDOW IS NEVER THIS REFUSAL'S: the segment is held on its
	// window before the resume is judged ([Engine.heldByBudget]), and one
	// that reaches here has turned over since.
	if _, ok := resumeRefusal(toolloop.ErrBudgetExhausted); ok {
		t.Error("a spent window was read as the provider's wait")
	}
	if _, ok := resumeRefusal(errors.New("server error")); ok {
		t.Error("a failure that is not a wait was read as one")
	}

	// AND ONCE COLLECTED, the completion's resume is refused on the limit its
	// review's chain knows, and its seat parked on it.
	refusal, ok := e.collectedRefusal(t.Context(), e.Company(), "swe")
	if !ok || refusal.Condition != waitProvider || !refusal.Until.Equal(retry) {
		t.Fatalf("collectedRefusal = %+v (%v), want a refusal waiting on %q until %v",
			refusal, ok, waitProvider, retry)
	}
	if e.providerParks.parking(seatIn(t, e, "swe")) == nil {
		t.Error("a collected completion refused on its models' limit left its seat unparked")
	}
}

// A COMPLETION THAT ARRIVES DURING A LIMIT ITS MODELS KNOW IS COLLECTED ONCE,
// PARKS ITS SEAT, IS NEVER HANDED BACK, AND RESUMES ONCE WHEN IT LIFTS.
//
// The chain already knows its limit, yet the completion is collected (its job is
// over; its tokens are charged where it ran, its box paused). Its resume is
// refused before its turn runs and the seat parked (`seat_provider_parked`). A
// later announcement is refused before the gate, acknowledged, never handed back.
// Mutation: gate-refuse it again (seatCannotRun) and it is never collected; ask
// nothing once collected (resumeTurn) and its turn runs into the limit; drop the
// park from collectedRefusal and nothing is published or probed; NAK in
// controlOutcome and the held announcement is handed back.
func TestACompletionDuringALimitIsNeverHandedBackAndResumesOnce(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	var admitted atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle, conversation string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle, conversation, route)
			if err == nil {
				admitted.Add(1)
			}
			return ctx, release, err
		}
	}
	e := r.start(t)
	alarms := &fakeAlarms{}
	e.providerParks.after = alarms.after
	entry := &spentEntry{retry: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	entry.limited.Store(true)
	withModels(t, e, entry)
	var mu sync.Mutex
	var parks []types.SeatProviderParked
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if parked, ok := events.DataAs[*types.SeatProviderParked](ev); ok && parked != nil {
			mu.Lock()
			parks = append(parks, *parked)
			mu.Unlock()
		}
	})
	published := func() []types.SeatProviderParked {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(parks)
	}
	agentID, _ := org.DeriveAgentID("Nimbus", "swe")
	scope := coord.AgentScope(agentID.String())
	r.seedRun(t, e, "t1")
	box := r.provider.Box(r.boxID)

	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test",
		InputTokens: 70, OutputTokens: 30})
	eventually(t, "the seat to be parked on the limit its review's models know", func() bool {
		return len(published()) > 0
	})
	eventually(t, "the collected job to be given back to running", func() bool {
		run := r.run(t, e, "t1")
		return run.Status == sandbox.StatusRunning && run.Launch.Collected
	})
	if got := published(); got[0].AgentHandle != "swe" ||
		!slices.Equal(got[0].ProviderKeys, []string{"default"}) || !got[0].RetryAt.Equal(entry.retry) {
		t.Fatalf("published %+v, want the swe seat parked on default until %v", got, entry.retry)
	}
	if e.providerParks.parking(seatIn(t, e, "swe")) == nil || len(alarms.live()) != 1 {
		t.Fatalf("the seat is parked %v with %d probe alarms, want it parked and probed",
			e.providerParks.parking(seatIn(t, e, "swe")) != nil, len(alarms.live()))
	}
	if n := r.collected.Load(); n != 1 {
		t.Fatalf("the box was collected %d times, want once", n)
	}
	used, err := e.backends.Fleet.Used(t.Context(), scope, coord.WindowsAt(time.Now(), time.UTC))
	if err != nil {
		t.Fatalf("Used: %v", err)
	}
	if got := used.In(period.Day).Used; got != 100 {
		t.Fatalf("the seat's day holds %d tokens, want the run's 100, charged as it was collected", got)
	}
	if !box.Paused() {
		t.Fatal("the collected box was not paused")
	}
	if n := entry.calls.Load(); n != 0 {
		t.Fatalf("the model was asked %d times, want none: the resumed turn ran into a limit "+
			"its entry already knew", n)
	}

	// HELD HERE: the waiter's polls leave the box asleep and collect nothing,
	// and an announcement is refused before the gate and acknowledged.
	keepalives, settled := box.Keepalives(), admitted.Load()
	time.Sleep(time.Second)
	if n := r.collected.Load(); n != 1 || !box.Paused() || box.Keepalives() != keepalives {
		t.Errorf("while held the box was collected %d times, paused %v and kept alive %d more "+
			"times, want it collected once and left asleep", n, box.Paused(), box.Keepalives()-keepalives)
	}
	run := r.run(t, e, "t1")
	payload := types.ExecutorFinished{Agent: run.AgentID, AgentHandle: run.AgentHandle,
		RoleName: run.Role, TurnID: run.TurnID, WorkKey: run.WorkKey, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, Harness: run.Harness}
	err = e.sandbox.Load().coordinator.OnCompleted(t.Context(), payload,
		events.New(payload, events.TraceContext{}))
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition != waitProvider {
		t.Fatalf("an announcement of the held job = %v, want the limit's refusal", err)
	}
	if got := controlOutcome(t.Context(), err).Outcome; got != queue.OutcomeAck {
		t.Fatalf("the control topic hands the held job back as %s, want ack", got)
	}
	if got := admitted.Load(); got != settled {
		t.Fatalf("%d resumes admitted while the job was held, want none", got-settled)
	}
	if deferred(t, e) {
		t.Fatal("a completion refused for a limit quiesced the seat's control topic")
	}

	// THE LIMIT LIFTS: the probe finds the model answering, and the park's
	// release re-fires the job, which is collected again to be resumed, once.
	// The waiters are stopped first, so nothing but that signal brings the job
	// back, and every admission counted is the release's.
	stopWaiter(e)
	settled = admitted.Load()
	entry.limited.Store(false)
	alarms.live()[0].fire()
	eventually(t, "the run to leave running once the limit lifted", func() bool {
		return r.run(t, e, "t1").Status != sandbox.StatusRunning
	})
	time.Sleep(500 * time.Millisecond)
	if got := admitted.Load() - settled; got != 1 {
		t.Fatalf("the held job was admitted %d times once the limit lifted, want once", got)
	}
	if n := r.collected.Load(); n != 2 {
		t.Fatalf("the box was collected %d times, want once while the limit held and once "+
			"to resume", n)
	}
}

// midTurnLimit is a model whose entry has reported nothing yet, so the turn
// gate admits a resume, and whose call refuses for a usage limit while the case
// holds it: the limit a resumed turn meets a round in.
type midTurnLimit struct {
	limited atomic.Bool
	retry   time.Time
	calls   atomic.Int32
}

func (*midTurnLimit) Model() string { return "opus" }

func (m *midTurnLimit) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	call := m.calls.Add(1)
	if m.limited.Load() {
		return nil, m.limitedErr(call)
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

// limitedErr is the refusal the model makes while it is limited: the first
// names retry, and every later one an hour past it, as a vendor whose window
// outlasts the hour the backend benches an entry for does when the probe asks
// again. A later refusal naming a later instant is what makes the probe's
// renewal news, which a park publishes once per change.
func (m *midTurnLimit) limitedErr(call int32) error {
	retry := m.retry
	if call > 1 {
		retry = retry.Add(time.Hour)
	}
	return &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: retry,
		Err: errors.New("You've hit your 5-hour limit")}
}

// limitedResume is a drain rig whose swe seat's launch has finished and
// whose resume has met the model's limit mid-turn, with the waiter stopped so
// nothing but a signal can bring the kept completion back: the engine, the
// model, how many times the gate admitted a resume, how many it had once the
// refusal settled, the provider park alarms the case fires, and every
// seat_provider_parked the engine published.
type limitedResume struct {
	r        *drainRig
	e        *Engine
	model    *midTurnLimit
	admitted *atomic.Int32
	settled  int32
	alarms   *fakeAlarms

	mu     sync.Mutex
	parked []types.SeatProviderParked
}

// newLimitedResume is a [limitedResume] whose model is limited until retryIn
// from now: the instant its refusal names, which the resume's own hold waits
// for.
func newLimitedResume(t *testing.T, retryIn time.Duration) *limitedResume {
	t.Helper()
	r := newDrainRig(t)
	var admitted atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle, conversation string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle, conversation, route)
			if err == nil {
				admitted.Add(1)
			}
			return ctx, release, err
		}
	}
	e := r.start(t)
	lr := &limitedResume{r: r, e: e, admitted: &admitted, alarms: &fakeAlarms{}}
	// THE PARK'S PROBE IS FIRED BY THE CASE, never by time.
	e.providerParks.after = lr.alarms.after
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if parked, ok := events.DataAs[*types.SeatProviderParked](ev); ok && parked != nil {
			lr.mu.Lock()
			lr.parked = append(lr.parked, *parked)
			lr.mu.Unlock()
		}
	})
	lr.model = &midTurnLimit{retry: time.Now().Add(retryIn)}
	lr.model.limited.Store(true)
	withModels(t, e, lr.model)
	r.seedRun(t, e, "t1")
	handlers := e.backends.Queue.InFlightCount()

	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	// THE REAL RESUME, into the limit: the run is claimed, collected, and
	// its turn calls the model, which refuses.
	eventually(t, "the resumed turn to meet the model's limit", func() bool {
		return lr.model.calls.Load() > 0
	})
	stopWaiter(e)
	eventually(t, "the refused resume's handlers to return", func() bool {
		return e.backends.Queue.InFlightCount() == handlers
	})
	eventually(t, "the run to be given back to running", func() bool {
		return r.run(t, e, "t1").Status == sandbox.StatusRunning
	})
	lr.settled = admitted.Load()
	// NEVER HANDED BACK: a NAKed completion comes back on the broker's
	// backoff, a second doubling, with nothing announcing it.
	time.Sleep(2500 * time.Millisecond)
	if got := admitted.Load(); got != lr.settled {
		t.Fatalf("admissions rose from %d to %d with the waiter stopped: the refused "+
			"completion was handed back to the broker", lr.settled, got)
	}
	if deferred(t, e) {
		t.Fatal("a completion refused for a limit quiesced the seat's control topic")
	}
	return lr
}

// parks is every seat_provider_parked the engine published, in order.
func (lr *limitedResume) parks() []types.SeatProviderParked {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return slices.Clone(lr.parked)
}

// probe fires the one probe alarm the seat's park armed, as its instant would.
func (lr *limitedResume) probe(t *testing.T) {
	t.Helper()
	live := lr.alarms.live()
	if len(live) != 1 {
		t.Fatalf("%d probe alarms armed, want the swe park's", len(live))
	}
	live[0].fire()
}

// announce offers the run's completion to the coordinator as the waiter's
// next announcement would, and returns what the coordinator answered.
func (lr *limitedResume) announce(t *testing.T) error {
	t.Helper()
	run := lr.r.run(t, lr.e, "t1")
	payload := types.ExecutorFinished{Agent: run.AgentID, AgentHandle: run.AgentHandle,
		RoleName: run.Role, TurnID: run.TurnID, WorkKey: run.WorkKey, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, Harness: run.Harness}
	return lr.e.sandbox.Load().coordinator.OnCompleted(t.Context(), payload,
		events.New(payload, events.TraceContext{}))
}

// resumedOnce lifts the limit with lift, which must signal the wait itself,
// and holds the kept completion to one resume.
func (lr *limitedResume) resumedOnce(t *testing.T, lift func()) {
	t.Helper()
	lr.model.limited.Store(false)
	lift()
	eventually(t, "the kept completion to resume once the limit lifted", func() bool {
		return lr.admitted.Load() > lr.settled
	})
	eventually(t, "the run to leave running", func() bool {
		return lr.r.run(t, lr.e, "t1").Status != sandbox.StatusRunning
	})
	time.Sleep(time.Second)
	if got := lr.admitted.Load() - lr.settled; got != 1 {
		t.Fatalf("the kept completion resumed %d times after the limit lifted, want once", got)
	}
}

// A RESUMED TURN THAT MEETS THE LIMIT MID-TURN IS A WAIT, END TO END, AND IT
// PARKS ITS SEAT AS A DISPATCH WOULD. The model refuses a round in, the claim is
// given back and the completion acknowledged and kept, never handed back to the
// broker. No delivery of the seat's own met the limit (its run holds its mail),
// yet seat_provider_parked names the entry and when it is tried again, and the
// probe is armed for that instant; its release signals the wait and the
// completion resumes, once.
//
// Mutation: drop the refusal mapping from resumeTurn and the completion is
// NAKed into the limit; drop the park from it and nothing is published or
// armed; drop the signal from the park's release and nothing brings the kept
// completion back.
func TestAResumeThatMeetsALimitMidTurnWaitsForTheParksRelease(t *testing.T) {
	t.Parallel()
	lr := newLimitedResume(t, time.Hour)
	parks := lr.parks()
	if len(parks) != 1 || parks[0].AgentHandle != "swe" ||
		!slices.Equal(parks[0].ProviderKeys, []string{"default"}) || !parks[0].RetryAt.Equal(lr.model.retry) {
		t.Fatalf("the resume that met the limit published %+v, want the swe seat parked on "+
			"default until %v", parks, lr.model.retry.UTC())
	}
	lr.resumedOnce(t, func() { lr.probe(t) })
}

// A COMPLETION WHOSE RESUME PARKED ITS SEAT WAITS FOR THE PARK, NOT FOR ITS OWN
// INSTANT. The resume met the limit after its claim, so its node holds the job
// and the seat is parked. The probe at the named instant meets the limit again
// (the vendor's window outlasts the hour the backend benches an entry for), so
// the park stands, renewed: the waiter's next announcement is then refused at
// the gate, before its claim, with no collection, review or box, where the
// review's own chain, ready again, would admit it into the window the probe
// just found spent. The probe that finds the model answering releases the park,
// and the completion resumes once.
//
// Mutation: drop the park from the turn gate (seatCannotRun), and the
// announcement past the instant is admitted and reviewed into the limit.
func TestACompletionAtALimitWaitsForItsParkNotItsInstant(t *testing.T) {
	t.Parallel()
	lr := newLimitedResume(t, 3*time.Second)
	lr.probe(t)
	if parks := lr.parks(); len(parks) != 2 {
		t.Fatalf("%d parks published, want the resume's and the probe's renewal", len(parks))
	}
	time.Sleep(time.Until(lr.model.retry) + 100*time.Millisecond)

	calls := lr.model.calls.Load()
	err := lr.announce(t)
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition != waitProvider {
		t.Fatalf("the announcement past the instant = %v, want the park's refusal", err)
	}
	if got := lr.admitted.Load(); got != lr.settled {
		t.Fatalf("the announcement past the instant was admitted (%d admissions, want %d) "+
			"while the park stood", got, lr.settled)
	}
	if got := lr.model.calls.Load(); got != calls || lr.r.collected.Load() != 1 {
		t.Fatalf("the model was asked %d more times and the box collected %d times: the "+
			"parked seat's job was run into the limit", got-calls, lr.r.collected.Load())
	}
	lr.resumedOnce(t, func() { lr.probe(t) })
}

// A PUBLISHED COMPANY BUILDS EVERY ENTRY AFRESH, so a completion kept for a
// limit is re-checked under it at once rather than at the waiter's next
// announcement or the park's probe: one its resume met mid-turn, and one whose
// collected resume met a limit its entry already knew. Each parked its seat,
// and the new models release the park and re-fire the job.
//
// Mutation: drop the release of a park whose models changed from the converge
// (reconcileProviderParks), and each parked seat's completion waits for a probe.
func TestAPublishedCompanyResumesACompletionKeptForALimit(t *testing.T) {
	t.Parallel()
	t.Run("met by its resume", func(t *testing.T) {
		t.Parallel()
		lr := newLimitedResume(t, time.Hour)
		lr.resumedOnce(t, func() {
			withModels(t, lr.e, lr.model)
			lr.e.followCompany(t.Context(), lr.e.Company())
		})
	})
	t.Run("known once collected", func(t *testing.T) {
		t.Parallel()
		r := newDrainRig(t)
		var admitted atomic.Int32
		r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
			return func(ctx context.Context, handle, conversation string, route sandbox.ResumeRoute) (context.Context, func(), error) {
				ctx, release, err := next(ctx, handle, conversation, route)
				if err == nil {
					admitted.Add(1)
				}
				return ctx, release, err
			}
		}
		e := r.start(t)
		e.providerParks.after = (&fakeAlarms{}).after
		entry := &spentEntry{retry: time.Now().Add(time.Hour)}
		entry.limited.Store(true)
		withModels(t, e, entry)
		r.seedRun(t, e, "t1")
		r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
		eventually(t, "the collected completion to park its seat on the limit", func() bool {
			return e.providerParks.parking(seatIn(t, e, "swe")) != nil
		})
		eventually(t, "the collected job to be given back to running", func() bool {
			run := r.run(t, e, "t1")
			return run.Status == sandbox.StatusRunning && run.Launch.Collected
		})
		stopWaiter(e)
		time.Sleep(time.Second)
		settled := admitted.Load()

		withModels(t, e, answers{})
		e.followCompany(t.Context(), e.Company())
		eventually(t, "the kept completion to resume under the published company", func() bool {
			return r.run(t, e, "t1").Status != sandbox.StatusRunning
		})
		if got := admitted.Load() - settled; got != 1 {
			t.Fatalf("the kept completion was admitted %d times under the new company, want once", got)
		}
	})
}

// countedLimit is a [knownLimit] that counts the calls that reached it, none of
// which should: its entry already knows its limit.
type countedLimit struct {
	knownLimit
	calls atomic.Int32
}

func (m *countedLimit) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	m.calls.Add(1)
	return m.knownLimit.Complete(ctx, req)
}

// AN APPLY THAT CARRIES ITS ENTRY'S SIT-OUT PARKS A SEAT WHOSE MAIL ITS RUN HOLDS
// AGAIN AT ONCE. A published company releases every park decided under the
// models it replaces and re-fires the job; its entry, unchanged by the apply,
// keeps the sit-out its sign-in serves (harness.Plans), so the job is collected
// again, its resume refused on that limit before its turn runs, and the seat
// parked once more, probe armed: no review, no model call. Nothing else re-parks
// such a seat (the redelivery that re-parks a dispatched seat never comes to one
// whose mail its run holds); it would read released, and its job would be
// reviewed into the vendor's window when the sit-out ends.
//
// Mutation: drop the park from collectedRefusal, and the seat stays released
// after the apply.
func TestAnApplyThatCarriesTheSitOutParksTheSeatAgain(t *testing.T) {
	t.Parallel()
	lr := newLimitedResume(t, time.Hour)
	swe := seatIn(t, lr.e, "swe")
	if lr.e.providerParks.parking(swe) == nil {
		t.Fatal("the premise: the resume parked the seat")
	}

	// THE APPLY: an entry that knows its limit stands for the one the apply
	// carried the sit-out to.
	carried := &countedLimit{knownLimit: knownLimit{
		retry: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}}
	withModels(t, lr.e, carried)
	lr.e.followCompany(t.Context(), lr.e.Company())
	eventually(t, "the seat to be parked again on the carried limit", func() bool {
		parks := lr.parks()
		return len(parks) == 2 && parks[1].RetryAt.Equal(carried.retry)
	})
	if parking := lr.e.providerParks.parking(swe); parking == nil ||
		parking.basis.models != lr.e.Company().Models {
		t.Fatal("the seat is not parked under the published company's models")
	}
	if live := lr.alarms.live(); len(live) != 1 {
		t.Fatalf("%d probe alarms armed, want the new park's", len(live))
	}
	eventually(t, "the job to be held collected", func() bool {
		run := lr.r.run(t, lr.e, "t1")
		return run.Status == sandbox.StatusRunning && run.Launch.Collected
	})
	time.Sleep(500 * time.Millisecond)
	if got := carried.calls.Load(); got != 0 {
		t.Errorf("the published company's model was asked %d times, want none: the review "+
			"ran into the carried limit", got)
	}
	if got := lr.admitted.Load() - lr.settled; got != 1 {
		t.Errorf("the job was admitted %d times after the apply, want once: the collection "+
			"that found the carried limit", got)
	}
}

// A PERSON WHO STOPS A SEAT WHOSE FINISHED RUN WAITS FOR A LIMIT STOPS IT NOW.
// The run's resume met the limit after its claim, so its node holds the job and
// the seat is parked on the limit, up to an hour on its probe. The stop ends the
// hold, and the turn gate admits the stopped seat's completion past the park,
// because the turn it re-enters is closed as stopped at its first round, before
// any model call: the run ends at once, inside the window, and the model is not
// asked again.
//
// Mutation: refuse a stopped seat's completion at the gate as any other
// (seatCannotRun), and the run waits for the park while the stop stands.
func TestAStopReachesACompletionWaitingForALimit(t *testing.T) {
	t.Parallel()
	lr := newLimitedResume(t, time.Hour)
	calls := lr.model.calls.Load()
	swe := seatIn(t, lr.e, "swe")
	lr.e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: swe,
		Pause: &coord.SeatPause{Seat: swe, By: "ana", StopRunning: true}})
	store := sandbox.NewCoordStore(lr.e.backends.Fleet)
	eventually(t, "the stopped seat's waiting run to end", func() bool {
		_, found, err := store.Get(t.Context(), "t1")
		return err == nil && !found
	})
	if got := lr.admitted.Load() - lr.settled; got != 1 {
		t.Errorf("the stopped seat's completion was admitted %d times, want once", got)
	}
	if got := lr.model.calls.Load(); got != calls {
		t.Errorf("the stopped turn asked the model %d more times, want none", got-calls)
	}
}

// limitOnReturn is a model whose call is out until the case lets it go, and
// comes back refused for a usage limit: the review a person's stop lands under.
type limitOnReturn struct {
	retry   time.Time
	entered chan struct{}
	release chan struct{}
}

func (*limitOnReturn) Model() string { return "opus" }

func (m *limitOnReturn) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	select {
	case m.entered <- struct{}{}:
	default:
	}
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: m.retry,
		Err: errors.New("You've hit your 5-hour limit")}
}

// A STOP THAT LANDS WHILE A RESUME'S CALL IS OUT DECIDES THE WAIT THAT CALL
// MEETS. A person stops the seat while its finished run's review waits on the
// model, and the call comes back refused for the model's limit. The stop leaves
// a claimed run to its turn, whose fence closes it at its next round, and there
// is none: reverted on the limit, the job would be held until the limit's
// instant (up to the hour an entry is benched for) with the stopped seat parked
// and probed. The resume is settled as the stopped turn it is: the run ends at
// once, the turn is said stopped, and nothing is parked.
//
// Mutation: turn the limit into the resume's refusal without asking whether
// the seat is stopping (resumeTurn), and the stopped seat is parked and its run
// held until the instant.
func TestAStopDecidesTheLimitAResumesCallMeets(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	e := r.start(t)
	e.providerParks.after = (&fakeAlarms{}).after
	model := &limitOnReturn{retry: time.Now().Add(time.Hour), entered: make(chan struct{}, 1),
		release: make(chan struct{})}
	withModels(t, e, model)
	var parked, stopped atomic.Int32
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if p, ok := events.DataAs[*types.SeatProviderParked](ev); ok && p != nil {
			parked.Add(1)
		}
		if p, ok := events.DataAs[*types.AgentTurnStopped](ev); ok && p != nil {
			stopped.Add(1)
		}
	})
	r.seedRun(t, e, "t1")
	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	select {
	case <-model.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the resumed turn never called the model")
	}

	swe := seatIn(t, e, "swe")
	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: swe,
		Pause: &coord.SeatPause{Seat: swe, By: "ana", StopRunning: true}})
	close(model.release)
	store := sandbox.NewCoordStore(e.backends.Fleet)
	eventually(t, "the stopped seat's run to end", func() bool {
		_, found, err := store.Get(t.Context(), "t1")
		return err == nil && !found
	})
	eventually(t, "the resumed turn to be said stopped", func() bool { return stopped.Load() > 0 })
	if n := parked.Load(); n != 0 || e.providerParks.parking(swe) != nil {
		t.Fatalf("the stopped seat was parked (%d parks published) on a limit its stop decided", n)
	}
	if n := r.collected.Load(); n != 1 {
		t.Errorf("the box was collected %d times, want once", n)
	}
}

// THE WAITS A STOP DECIDES ARE THE ONES A RESUME WOULD WAIT ON: a limit every
// model of its chain reported, and a budget that holds the segment (a window
// full, or too short for the launch every relaunch makes). Any other failure is
// a retry, whose next attempt the seat's fence closes as stopped at its first
// round, and a seat with no ceiling has no budget to wait on.
//
// Mutation: read the budget's refusal without asking whether the seat's window
// refuses (resumeWaits), and a stopped seat's uncapped segment is settled as
// though it waited.
func TestTheWaitsAStopDecidesAreTheResumesWaits(t *testing.T) {
	t.Parallel()
	limit := &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: time.Now().Add(time.Hour),
		Err: errors.New("You've hit your 5-hour limit")}
	limited := &chain.Error{Attempted: []string{"default"}, Failures: []error{limit}, Err: limit}
	halfLimited := &chain.Error{Attempted: []string{"default", "spare"},
		Failures: []error{limit, errors.New("server error")}, Err: errors.New("server error")}
	uncapped := newDrainRig(t).start(t)
	for name, tc := range map[string]struct {
		err   error
		waits bool
	}{
		"a limit every model reported":       {err: limited, waits: true},
		"a limit one model of two reported":  {err: halfLimited},
		"a failure that is not a wait":       {err: errors.New("the model provider did not answer")},
		"a budget on a seat with no ceiling": {err: toolloop.ErrBudgetExhausted},
	} {
		if got := uncapped.resumeWaits(t.Context(), uncapped.Company(), "swe", tc.err); got != tc.waits {
			t.Errorf("%s: resumeWaits = %v, want %v", name, got, tc.waits)
		}
	}
	capped := newDrainRig(t)
	capped.doc = cappedSWEDoc
	e := capped.start(t)
	if !e.resumeWaits(t.Context(), e.Company(), "swe", toolloop.ErrBudgetExhausted) {
		t.Error("a segment its seat's window holds is not a wait the stop decides")
	}
}

// A PARK TAKEN AFTER ITS SEAT LEFT THIS NODE IS UNDONE AT ONCE. A seat's
// release does not wait for the turn whose refusal parks the seat, so the park
// can land after the release has forgotten the seat's parks, and it outlived
// the seat here (the package doc's "A seat parked until an instant"). A budget
// park on a launch's refusal below its floor takes its recorded window with it.
// A park asked once the release ended its attachment is refused (the next
// case); the park undone here is one whose hold the queue took before the
// release's re-check, staged by handing the drained engine an attachment again.
//
// Mutation: drop the seat's re-check from providerPark, or from budgetPark,
// and that park, its probe or alarm, and its hold stay on a seat this node no
// longer holds; keep the budget park's window, and it outlives the park.
func TestAParkTakenAfterItsSeatLeftIsUndone(t *testing.T) {
	t.Parallel()
	// drained is a rig's engine once its drain has handed every seat back,
	// with the parks' alarms fired by the case, and an attachment of the
	// seat's inbox the park's hold is taken for.
	drained := func(t *testing.T, doc string) (*Engine, *fakeAlarms) {
		t.Helper()
		e, alarms := drainedEngine(t, doc)
		takeInbox(t, e, e.backends.Queue, seatIn(t, e, "swe"))
		return e, alarms
	}
	t.Run("a provider park", func(t *testing.T) {
		t.Parallel()
		e, alarms := drained(t, "")
		var released atomic.Int32
		e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
			if p, ok := events.DataAs[*types.SeatProviderReleased](ev); ok && p != nil {
				released.Add(1)
			}
		})
		refusal := &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: time.Now().Add(time.Hour),
			Err: errors.New("You've hit your 5-hour limit")}
		limit, limited := chain.Limited(&chain.Error{Attempted: []string{"default"},
			Failures: []error{refusal}, Err: refusal})
		if !limited {
			t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
		}
		if _, parked, err := e.providerPark(t.Context(), "swe", limit); err != nil || !parked {
			t.Fatalf("providerPark = %v, %v", parked, err)
		}
		if parking := e.providerParks.parking(seatIn(t, e, "swe")); parking != nil {
			t.Fatal("the park outlived its seat on this node")
		}
		if live := alarms.live(); len(live) != 0 {
			t.Fatalf("%d probe alarms armed for a seat this node no longer holds, want none", len(live))
		}
		if holds := inboxHolds(t, e, "swe"); slices.Contains(holds, pauseReasonProvider) {
			t.Fatalf("the seat's inbox is held %v by a park on a seat this node no longer holds", holds)
		}
		if got := released.Load(); got != 1 {
			t.Fatalf("%d releases published, want the one saying the seat was released", got)
		}
	})
	t.Run("a budget park", func(t *testing.T) {
		t.Parallel()
		e, alarms := drained(t, cappedSWEDoc)
		// THE SEAT'S DAY IS FULL, so its window refuses.
		if _, err := e.backends.Fleet.PostCharge(t.Context(), coord.AgentScope(seatIn(t, e, "swe").String()),
			1000, coord.WindowsAt(time.Now(), time.UTC)); err != nil {
			t.Fatalf("PostCharge: %v", err)
		}
		// A LAUNCH'S REFUSAL BELOW ITS FLOOR, whose park records the window it
		// waits on for the delivery whose launch it was.
		refused := &floorRefusal{floor: staticFloor(e.Company()), left: 0}
		delivery := []*events.Event{{ID: uuid.New(), Type: "task_assigned"}}
		if _, parked, err := e.budgetPark(t.Context(), "swe", delivery, refused); err != nil || !parked {
			t.Fatalf("budgetPark = %v, %v", parked, err)
		}
		if parking := e.budgetParks.parking(seatIn(t, e, "swe")); parking != nil {
			t.Fatal("the park outlived its seat on this node")
		}
		if waited := e.floorWaits.waited(seatIn(t, e, "swe"), deliveryOf(delivery)); len(waited) != 0 {
			t.Fatalf("the park left the window %+v for the delivery's next launch on a node that no longer "+
				"holds its seat", waited[0])
		}
		if live := alarms.live(); len(live) != 0 {
			t.Fatalf("%d reset alarms armed for a seat this node no longer holds, want none", len(live))
		}
		if holds := inboxHolds(t, e, "swe"); slices.Contains(holds, pauseReasonBudget) {
			t.Fatalf("the seat's inbox is held %v by a park on a seat this node no longer holds", holds)
		}
	})
}

// A RESUME'S PARK ON A SEAT THIS NODE HAS LET GO OF IS NO WARNING. A resume
// runs in no delivery, its review minutes long, so its seat can be released
// under it, and the park its limit then asks for names the attachment that
// release ended, which the queue refuses. That is the ordinary answer for a
// seat that moved, not a park that failed: said as a warning, every seat a
// drain hands on under a resume reads as a fault in the node's log.
//
// NOT PARALLEL: it reads the process's log, which [logging.Configure] owns,
// and Go resumes every parallel test only after the sequential ones end.
//
// Mutation: say every park a resume could not take as a warning, and this
// fails.
func TestAResumesParkOnASeatLetGoOfIsNoWarning(t *testing.T) {
	ctx := t.Context()
	logs := &syncBuffer{}
	logging.Configure(slog.LevelDebug, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	e, q := engineOn(t)
	id := seatIn(t, e, "ceo")
	// THE RELEASE: the inbox detached, then the engine told.
	if _, err := q.Detach(ctx, topics.AgentInbox(id), topics.AgentInboxGroup(id)); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	e.detachInbox(id)

	refusal := &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: time.Now().Add(time.Hour),
		Err: errors.New("You've hit your 5-hour limit")}
	limit, limited := chain.Limited(&chain.Error{Attempted: []string{"default"},
		Failures: []error{refusal}, Err: refusal})
	if !limited {
		t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
	}
	e.parkOnLimit(ctx, "ceo", limit)

	if n := countLogLines(logs, "seat_provider_park_attachment_ended"); n != 1 {
		t.Errorf("%d lines say the park met the attachment its seat's release ended, want 1", n)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "msg=seat_provider_park_") && !strings.Contains(line, "level=DEBUG") {
			t.Errorf("a park refused for a seat this node let go of was said above debug: %s", line)
		}
	}
}

// drainedEngine is a drain rig's engine once its drain has handed every seat
// back, with the parks' alarms fired by the case.
func drainedEngine(t *testing.T, doc string) (*Engine, *fakeAlarms) {
	t.Helper()
	r := newDrainRig(t)
	r.doc = doc
	e := r.start(t)
	alarms := &fakeAlarms{}
	e.providerParks.after, e.budgetParks.after = alarms.after, alarms.after
	e.Drain(t.Context())
	if _, held := e.launchFence("swe"); held {
		t.Fatal("the premise: the drained node still holds the seat")
	}
	return e, alarms
}

// A PARK ASKED ONCE ITS SEAT'S RELEASE HAS DETACHED THE INBOX IS REFUSED, and
// records nothing: its hold names the attachment the seat held here, which the
// release ended, and the queue refuses a hold for an ended attachment rather
// than carry it onto the inbox's next attachment on this node. Taken anyway, as
// it was when holds were keyed by the subscription alone, the hold landed on
// the inbox this node attaches if it takes the seat back, which then consumed
// nothing, with no park left to lift it.
//
// Mutation: take a park's hold for the inbox's current attachment rather than
// the one the seat held, and each park is taken, its hold left on the inbox.
func TestAParkAskedAfterItsSeatsReleaseIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("a provider park", func(t *testing.T) {
		t.Parallel()
		e, alarms := drainedEngine(t, "")
		refusal := &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: time.Now().Add(time.Hour),
			Err: errors.New("You've hit your 5-hour limit")}
		limit, limited := chain.Limited(&chain.Error{Attempted: []string{"default"},
			Failures: []error{refusal}, Err: refusal})
		if !limited {
			t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
		}
		_, parked, err := e.providerPark(t.Context(), "swe", limit)
		if !errors.Is(err, queue.ErrAttachmentGone) || parked {
			t.Fatalf("providerPark = %v, %v; want it refused, the seat's attachment ended", parked, err)
		}
		if parking := e.providerParks.parking(seatIn(t, e, "swe")); parking != nil {
			t.Fatal("a park refused its hold was recorded")
		}
		if live := alarms.live(); len(live) != 0 {
			t.Fatalf("%d probe alarms armed for a park refused its hold, want none", len(live))
		}
		if holds := inboxHolds(t, e, "swe"); len(holds) != 0 {
			t.Fatalf("the seat's inbox is held %v after its release", holds)
		}
	})
	t.Run("a budget park", func(t *testing.T) {
		t.Parallel()
		e, alarms := drainedEngine(t, cappedSWEDoc)
		if _, err := e.backends.Fleet.PostCharge(t.Context(), coord.AgentScope(seatIn(t, e, "swe").String()),
			1000, coord.WindowsAt(time.Now(), time.UTC)); err != nil {
			t.Fatalf("PostCharge: %v", err)
		}
		refused := &floorRefusal{floor: staticFloor(e.Company()), left: 0}
		delivery := []*events.Event{{ID: uuid.New(), Type: "task_assigned"}}
		_, parked, err := e.budgetPark(t.Context(), "swe", delivery, refused)
		if !errors.Is(err, queue.ErrAttachmentGone) || parked {
			t.Fatalf("budgetPark = %v, %v; want it refused, the seat's attachment ended", parked, err)
		}
		if parking := e.budgetParks.parking(seatIn(t, e, "swe")); parking != nil {
			t.Fatal("a park refused its hold was recorded")
		}
		if waited := e.floorWaits.waited(seatIn(t, e, "swe"), deliveryOf(delivery)); len(waited) != 0 {
			t.Fatalf("a park refused its hold left the window %+v for the delivery's next launch", waited[0])
		}
		if live := alarms.live(); len(live) != 0 {
			t.Fatalf("%d reset alarms armed for a park refused its hold, want none", len(live))
		}
		if holds := inboxHolds(t, e, "swe"); len(holds) != 0 {
			t.Fatalf("the seat's inbox is held %v after its release", holds)
		}
	})
}
