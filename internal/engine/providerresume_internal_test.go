package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RESUME NEVER RUNS INTO A LIMIT ITS MODELS ALREADY KNOW, and one that meets
// a limit it did not know waits for it: neither is a failure.
//
// What a running engine did: a coding run's completion reached a seat whose
// models were all at their usage limit, the resume ran a turn into the limit,
// the turn was reverted, and the control topic NAKed the completion, which the
// broker redelivered into the same limit 25 times in about ten minutes.

// spentEntry is a model whose CLI already reported its usage limit, as
// cliagent's entry knows it ([chain.Unready]), until the case lifts it. calls
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
	spent := cliProvider(t, chainMember{key: "default", agent: "claude-code", model: "opus", spent: true})
	retry, _, limited := llm.LimitedUntil(spent.SittingOut())
	if !limited {
		t.Fatalf("the premise: the spent entry sits out nothing (%v)", spent.SittingOut())
	}
	withLaunchModels(t, e, spent)
	turns := e.node.InFlightTurns()

	_, release, err := e.admitResume(t.Context(), "swe", sandbox.ResumeAnswer)
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
	if got := controlOutcome(err).Outcome; got != queue.OutcomeAck {
		t.Errorf("the control topic hands a limited resume back as %s, want ack", got)
	}

	// A COMPLETION IS ADMITTED, to be collected before its limit is asked.
	_, release, err = e.admitResume(t.Context(), "swe", sandbox.ResumeCompletion)
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
// The seat's review's chain is an entry that already knows its limit, as every
// seat's on a shared plan does once the first run that spent it is collected.
// The completion is admitted and collected all the same: its job is over, so
// its tokens are charged in the window it ran in and its box is paused. Its
// resume is then refused on the limit before its turn runs, held on this node,
// and the seat is parked on the limit (`seat_provider_parked`, the chain's
// probe). An announcement meanwhile is refused before the gate and the claim
// and acknowledged, never handed back to the broker, and the waiter leaves the
// box asleep. When the probe finds the model answering, the park's release
// re-fires the job, which is collected again and resumed, once.
//
// Mutation: refuse the completion at the gate for its known limit again
// (seatCannotRun), and it is never collected while the limit holds; ask it
// nothing once collected (resumeTurn), and its turn runs into the limit; drop
// the park from collectedRefusal, and nothing is published or probed; NAK a
// refusal in controlOutcome, and the held announcement is handed back.
func TestACompletionDuringALimitIsNeverHandedBackAndResumesOnce(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	var admitted atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle, route)
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
	payload := types.SandboxRunCompleted{Agent: run.AgentID, AgentHandle: run.AgentHandle,
		RoleName: run.Role, TurnID: run.TurnID, WorkKey: run.WorkKey, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, CodingAgent: run.CodingAgent}
	err = e.sandbox.Load().coordinator.OnCompleted(t.Context(), payload,
		events.New(payload, events.TraceContext{}))
	var refused *sandbox.TurnRefused
	if !errors.As(err, &refused) || refused.Condition != waitProvider {
		t.Fatalf("an announcement of the held job = %v, want the limit's refusal", err)
	}
	if got := controlOutcome(err).Outcome; got != queue.OutcomeAck {
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
	stopWaiters(e)
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
	m.calls.Add(1)
	if m.limited.Load() {
		return nil, m.limitedErr()
	}
	return &llm.Completion{Content: "done", Model: "opus"}, nil
}

// limitedErr is the refusal the model makes while it is limited.
func (m *midTurnLimit) limitedErr() error {
	return &llm.Error{Kind: llm.KindRateLimit, Key: "default", RetryAt: m.retry,
		Err: errors.New("You've hit your 5-hour limit")}
}

// limitedResume is a drain rig whose swe seat's coding run has finished and
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
		return func(ctx context.Context, handle string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			ctx, release, err := next(ctx, handle, route)
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
	stopWaiters(e)
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
// next poll would, and returns what the coordinator answered.
func (lr *limitedResume) announce(t *testing.T) error {
	t.Helper()
	run := lr.r.run(t, lr.e, "t1")
	payload := types.SandboxRunCompleted{Agent: run.AgentID, AgentHandle: run.AgentHandle,
		RoleName: run.Role, TurnID: run.TurnID, WorkKey: run.WorkKey, LaunchID: run.LaunchID,
		SandboxID: run.SandboxID, CodingAgent: run.CodingAgent}
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
// PARKS ITS SEAT AS A DISPATCH WOULD. The real resumer runs the real turn, the
// model refuses a round in, the claim is given back and the completion
// acknowledged and kept, and it is never handed back to the broker. The seat's
// mail is held by its run, so no delivery of its own ever met the limit, and
// still the seat is parked on it: seat_provider_parked names the entry and when
// it is tried again, and the chain's probe is armed for that instant. When the
// probe finds the model answering, the release signals the wait and the
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
// until the instant the limit named, and the seat is parked. The probe at that
// instant meets the limit again (the vendor's window outlasts the hour the
// backend benches an entry for), so the park stands, renewed, and the instant
// passes: the waiter's next announcement is then refused at the gate, before
// its claim, with no collection, no review and no box, where the review's own
// chain, ready again, admitted it into the window the probe had just found
// spent. The probe that finds the model answering releases the park, and the
// completion resumes once.
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
// limit is re-checked under it at once rather than at the waiter's next poll or
// the park's probe: one its resume met mid-turn, and one whose collected
// resume met a limit its entry already knew. Each parked its seat, and the new
// models release the park and re-fire the job.
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
			return func(ctx context.Context, handle string, route sandbox.ResumeRoute) (context.Context, func(), error) {
				ctx, release, err := next(ctx, handle, route)
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
		stopWaiters(e)
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
// AGAIN AT ONCE. The seat's resume met the limit mid-turn and parked it, and its
// node holds the job. A published company releases every park decided under the
// models it replaces and re-fires the job; its entry, left unchanged by the
// apply, keeps the sit-out its predecessor was serving (carrySitOuts), so the job
// is collected again and its resume refused on that limit before its turn runs,
// and the seat is parked on it once more, its probe armed: no review, no model
// call, and no job collected and reviewed into the vendor's window once the
// sit-out ends. Before, nothing parked such a seat again (the redelivery that
// re-parks a dispatched seat never comes to one whose mail its run holds): it
// read released, left the probe, and its job was reviewed into the window when
// the sit-out ended.
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

// A PARK TAKEN AFTER ITS SEAT LEFT THIS NODE IS UNDONE AT ONCE. A seat's
// release does not wait for the turn whose refusal parks the seat: a lost
// lease detaches the inbox under a running delivery, a turn refused mid-flight
// parks once the turn has run, and a resume runs in no delivery at all, its
// review minutes long, so the park can land after the release has forgotten
// the seat's parks. It outlived the seat here: a provider park's probe went on
// calling the models and publishing the seat parked while a peer ran it, a
// budget park's alarm fired into a seat a peer ran, and either one's hold,
// taken after the detach that drops holds, kept the inbox shut on this node.
//
// Mutation: drop the seat's re-check from providerPark, or from budgetPark,
// and that park, its probe or alarm, and its hold stay on a seat this node no
// longer holds.
func TestAParkTakenAfterItsSeatLeftIsUndone(t *testing.T) {
	t.Parallel()
	// drained is a rig's engine once its drain has handed every seat back,
	// with the parks' alarms fired by the case.
	drained := func(t *testing.T, doc string) (*Engine, *fakeAlarms) {
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
		if _, parked, err := e.budgetPark(t.Context(), "swe"); err != nil || !parked {
			t.Fatalf("budgetPark = %v, %v", parked, err)
		}
		if parking := e.budgetParks.parking(seatIn(t, e, "swe")); parking != nil {
			t.Fatal("the park outlived its seat on this node")
		}
		if live := alarms.live(); len(live) != 0 {
			t.Fatalf("%d reset alarms armed for a seat this node no longer holds, want none", len(live))
		}
		if holds := inboxHolds(t, e, "swe"); slices.Contains(holds, pauseReasonBudget) {
			t.Fatalf("the seat's inbox is held %v by a park on a seat this node no longer holds", holds)
		}
	})
}
