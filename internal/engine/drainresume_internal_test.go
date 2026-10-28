package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A DRAINING NODE STARTS NO TURN THROUGH A CODING RUN'S COMPLETION.
//
// What a running engine did: the drain closed the node's turn gate and quiesced
// every seat's inbox, the box's waiter went on running through it (its
// keepalive protects the box), and a completion it fired reached the seat's
// CONTROL topic, which nothing quiesced. The coordinator claimed the run,
// collected the box and resumed the turn on a node that was leaving: two full
// turns started minutes into a drain, each holding the drain open until it
// ended, and both abandoned when it did.
//
// These cases drive that road end to end: a real engine, the coordinator its
// own builder wires ([Engine.buildSandboxRuntime]) and its own waiter, firing
// completions to the seat's control topic as it does in production. The box
// and its coding agent are the in-process doubles, so the only thing standing
// in for production is what runs inside the box.

// drainRig is one engine over a store and a stream that outlive it, holding the
// swe seat with a coding run in its box.
type drainRig struct {
	b        config.Bootstrap
	provider *sandbox.FakeProvider
	runner   *sandbox.FakeRunner
	boxID    string

	// collected counts the box collections any engine on the rig made.
	collected atomic.Int32
}

func newDrainRig(t *testing.T) *drainRig {
	t.Helper()
	b := testBootstrap(t)
	dir := t.TempDir()
	b.Store.Path = filepath.Join(dir, "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(dir, "stream"))
	SeedStore(t, &b)
	r := &drainRig{b: b, provider: sandbox.NewFakeProvider(), runner: sandbox.NewFakeRunner("claude-code")}
	r.runner.CollectFunc = func(context.Context) error {
		r.collected.Add(1)
		return nil
	}
	box, err := r.provider.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	r.boxID = box.ID()
	return r
}

// start boots an engine on the rig, with the rig's box behind its coordinator
// and its waiter polling the box, and waits for it to hold the swe seat with
// its control topic attached.
func (r *drainRig) start(t *testing.T) *Engine {
	t.Helper()
	b := r.b
	e, err := New(t.Context(), Options{
		Bootstrap: &b, Company: parseCompany(t, companyWithoutSandboxDoc),
		SandboxPollInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: r.provider},
		Runners:   map[string]sandbox.Runner{"claude-code": r.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// THE ENGINE'S OWN COORDINATOR, built as a code-enabled company's is,
	// so the admission it resumes under is the one production wires.
	rt, err := e.buildSandboxRuntime(manager)
	if err != nil {
		t.Fatalf("buildSandboxRuntime: %v", err)
	}
	e.sandboxSeats = map[string]bool{}
	e.sandbox.Store(rt)
	// AND ITS WAITER, which a code-enabled company's boot starts beside it,
	// and which runs through a drain: its keepalive is what keeps the box.
	if err := e.startSandboxWaiter(t.Context()); err != nil {
		t.Fatalf("startSandboxWaiter: %v", err)
	}
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHeld(t, e, "swe")
	eventually(t, "the swe seat's control topic to be attached", func() bool {
		return e.sandboxSeatAttached("swe")
	})
	return e
}

// seedRun records a coding run on the swe seat whose job is running in the
// rig's box, suspended into the turn that launched it, as the node that
// launched it leaves it for the waiter. The job finishes when the case calls
// [sandbox.FakeRunner.Finish].
func (r *drainRig) seedRun(t *testing.T, e *Engine, turnID string) {
	t.Helper()
	ctx := t.Context()
	store := sandbox.NewCoordStore(e.backends.Fleet)
	agentID, _ := org.DeriveAgentID("Nimbus", "swe")
	if _, err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: turnID, AgentID: agentID.String(), AgentHandle: "swe", Role: "SWE",
		CodingAgent: "claude-code", CreatedAt: time.Now().UTC(),
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, turnID, sandbox.BoxRef{
		SandboxID: r.boxID, CommandID: "cmd-1", CodingAgent: "claude-code",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if suspended, err := store.MarkSuspended(ctx, turnID, sandbox.Suspension{State: json.RawMessage(
		`{"version":1,"pending_tool_call_id":"call-1","pending_tool_name":"` +
			builtin.RunSandboxTool + `"}`)}); err != nil || !suspended {
		t.Fatalf("MarkSuspended = (%v, %v), want the run open to the poll", suspended, err)
	}
}

func (r *drainRig) run(t *testing.T, e *Engine, turnID string) sandbox.PendingRun {
	t.Helper()
	run, found, err := sandbox.NewCoordStore(e.backends.Fleet).Get(t.Context(), turnID)
	if err != nil || !found {
		t.Fatalf("Get(%s) = (%v, %v), want the run", turnID, found, err)
	}
	return run
}

// deferred reports whether the swe seat's control attachment is quiesced, which
// only a deferral does to it, and leaves it as it found it. The queue answers
// the question only by resuming the attachment ([queue.EventQueue.Unquiesce]),
// so a quiesced one is quiesced again at once: the consume loop looks at the
// flag once per fetch interval, and a delivery it does take in between is
// deferred by a node that still refuses.
func deferred(t *testing.T, e *Engine) bool {
	t.Helper()
	agentID, _ := org.DeriveAgentID("Nimbus", "swe")
	control, group := topics.AgentControl(agentID), topics.AgentControlGroup(agentID)
	was, err := e.backends.Queue.Unquiesce(t.Context(), control, group)
	if err != nil {
		t.Fatalf("Unquiesce: %v", err)
	}
	if was {
		if _, err := e.backends.Queue.Quiesce(t.Context(), control, group); err != nil {
			t.Fatalf("Quiesce: %v", err)
		}
	}
	return was
}

// THE COMPLETION THAT ARRIVES DURING A DRAIN IS LEFT FOR THE SEAT'S NEXT
// HOLDER, AND THAT HOLDER RESUMES IT.
//
// Mutation: drop the admission from the coordinator's completion route, or the
// engine's gate from its wiring, and the draining node claims and collects the
// run; map a drain refusal to a Nak and the control attachment is never
// quiesced, so the broker hands the completion straight back to the node that
// is leaving for as long as it drains.
func TestADrainingNodeLeavesACompletionForTheSeatsNextHolder(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	e := r.start(t)
	r.seedRun(t, e, "t1")
	fired := firedCompletions(e)

	// A TURN IN FLIGHT KEEPS THE DRAIN OPEN, as the two did when the engine
	// was told to stop. Admitted as every turn is, so the drain waits on its
	// slot.
	_, finish, err := e.node.AdmitTurn(t.Context(), "swe")
	if err != nil {
		t.Fatalf("AdmitTurn: %v", err)
	}
	t.Cleanup(finish)
	drained := make(chan struct{})
	go func() {
		e.Drain(context.Background())
		close(drained)
	}()
	eventually(t, "the drain to close the node's turn gate", func() bool {
		_, release, err := e.node.AdmitTurn(t.Context(), "swe")
		if err == nil {
			release()
			return false
		}
		return errors.Is(err, node.ErrDraining)
	})
	handlers, turns := e.backends.Queue.InFlightCount(), e.node.InFlightTurns()

	// THE JOB FINISHES minutes into the drain, and the waiter fires.
	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	select {
	case <-fired:
	case <-time.After(20 * time.Second):
		t.Fatal("the waiter never fired the finished job's completion")
	}
	// DEFERRED, or collected by a node that went on to resume it: either
	// ends the wait, and only the first is right.
	eventually(t, "the draining node to defer the completion", func() bool {
		return r.collected.Load() > 0 || deferred(t, e)
	})
	eventually(t, "the deferred completion's handler to return", func() bool {
		return e.backends.Queue.InFlightCount() == handlers
	})
	if got := e.node.InFlightTurns(); got != turns {
		t.Errorf("turns in flight rose from %d to %d on a draining node", turns, got)
	}
	if got := r.collected.Load(); got != 0 {
		t.Fatalf("the draining node collected the box %d times", got)
	}
	if got := r.run(t, e, "t1"); got.Status != sandbox.StatusRunning || got.Collected {
		t.Fatalf("run = %q (collected %v), want it untouched for the seat's next holder",
			got.Status, got.Collected)
	}

	finish()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain never completed once its turn ended")
	}
	if got := r.run(t, e, "t1"); got.Status != sandbox.StatusRunning || got.Collected {
		t.Fatalf("after the drain, run = %q (collected %v), want it still left for the "+
			"seat's next holder", got.Status, got.Collected)
	}
	e.Stop(context.Background())

	// THE NEXT HOLDER, this node after its restart, collects the box the
	// drain left alone, on the completion its own waiter fires for the run
	// the drain left running.
	r.start(t)
	eventually(t, "the restarted node to collect the run the drain left", func() bool {
		return r.collected.Load() > 0
	})
}

// A REFUSED COMPLETION IS HANDED BACK BY THE CONDITION ITS REFUSAL NAMES.
//
// A node that is leaving defers: the attachment is quiesced for the rest of the
// drain and the completion waits at the head for the seat's next holder. A
// condition that clears by waiting is acked, which spends none of the broker's
// deliveries and quiesces nothing: the run is left unclaimed and the waiter
// announces it again on its next poll (TestAnUnclaimedCompletionIsAnnouncedAgain).
// A condition this build names nowhere yet, a provider's limit here, takes the
// waiting rule with no change to the hand-back, which is the point of keying it
// on the condition. Only a failure the refusal does not explain is a Nak.
//
// Mutation: key the hand-back on node.ErrDraining again and the provider limit
// is a Nak, walking a healthy completion to the dead-letter boundary one
// delivery at a time; defer every refusal and a seat whose ownership merely
// blipped quiesces its control topic for as long as this node holds it.
func TestARefusedCompletionIsHandedBackByItsCondition(t *testing.T) {
	t.Parallel()
	refusal := func(cond sandbox.Condition, cause error) error {
		return errors.Join(sandbox.ErrResumeUnavailable, &sandbox.TurnRefused{Cause: cause,
			Refusal: sandbox.Refusal{Condition: cond, Reason: cause.Error()}})
	}
	for name, tc := range map[string]struct {
		err  error
		want queue.Outcome
	}{
		"settled":                    {err: nil, want: queue.OutcomeAck},
		"the node is draining":       {err: refusal(waitDrain, node.ErrDraining), want: queue.OutcomeDefer},
		"the seat is not provable":   {err: refusal(waitOwnership, node.ErrSeatNotOwned), want: queue.OutcomeAck},
		"a provider's limit":         {err: refusal("provider_limit", errors.New("provider limit reached")), want: queue.OutcomeAck},
		"a refusal naming no wait":   {err: refusal("", errors.New("refused")), want: queue.OutcomeNak},
		"the seat is held elsewhere": {err: fmt.Errorf("%w: seat held elsewhere", sandbox.ErrResumeUnavailable), want: queue.OutcomeNak},
		"the store failed":           {err: errors.New("store unavailable"), want: queue.OutcomeNak},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := controlOutcome(tc.err).Outcome; got != tc.want {
				t.Errorf("controlOutcome(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// firedCompletions signals each completion the engine publishes, so a case can
// wait for the waiter to fire.
func firedCompletions(e *Engine) <-chan struct{} {
	fired := make(chan struct{}, 1)
	e.backends.Queue.AddPublishListener(func(_ context.Context, _ string, ev *events.Event) {
		if ev.Type == (types.SandboxRunCompleted{}).EventType() {
			select {
			case fired <- struct{}{}:
			default:
			}
		}
	})
	return fired
}

// A DRAIN WAITS FOR A RESUME ADMITTED BEFORE IT, and refuses the next one.
//
// The admission here is the one the coordinator's resume takes through the
// engine's gate ([Engine.admitResume]) on either route, a recorded answer's
// retry included, which runs on the coordinator's own timer and in no queue
// handler. A drain that waited only for the queue's handlers handed the seat
// back under it.
//
// Mutation: wait only for the queue's handlers in the node's drain and it
// returns with the resume still running; map a drain refusal to no condition
// and the next resume's wait names nothing.
func TestADrainWaitsForAResumeAdmittedBeforeIt(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	e := r.start(t)

	_, release, err := e.admitResume(t.Context(), "swe")
	if err != nil {
		t.Fatalf("admitResume on a serving node: %v", err)
	}
	t.Cleanup(release)
	drained := make(chan struct{})
	go func() {
		e.Drain(context.Background())
		close(drained)
	}()

	var refused *sandbox.TurnRefused
	eventually(t, "the drain to refuse the next resume", func() bool {
		_, next, err := e.admitResume(t.Context(), "swe")
		if err == nil {
			next()
			return false
		}
		return errors.As(err, &refused)
	})
	if refused.Condition != waitDrain || !errors.Is(refused, node.ErrDraining) {
		t.Fatalf("a resume on a draining node was refused as %+v, want it waiting on %q "+
			"with the node's drain as its cause", refused.Refusal, waitDrain)
	}
	select {
	case <-drained:
		t.Fatal("the drain finished while a resume admitted before it was still running")
	case <-time.After(300 * time.Millisecond):
	}
	if held := e.node.Host().Held(); len(held) != 1 {
		t.Fatalf("the drain gave the seat back under a running resume: holds %v", held)
	}

	release()
	select {
	case <-drained:
	case <-time.After(30 * time.Second):
		t.Fatal("the drain never completed once the resume ended")
	}
}

// A RESUMED TURN TAKES A SLOT UNDER node.max_concurrent.
//
// The ceiling is what the node is sized by, and a resumed turn is a full turn:
// model calls, tool loops, a coding CLI. With the node at its ceiling, a
// completion waits for a slot before it claims the run, and runs the moment
// one frees.
//
// THE WAIT IS OBSERVED, NOT SLEPT ON: the completion's handler is seen parked
// for the slot ([node.Node.TurnsWaiting]), which it can only be before its
// claim, and an ungated completion is seen collected instead; either ends the
// wait, and only the first is right.
//
// Mutation: drop the gate from the engine's coordinator and the completion is
// collected and resumed beside the turn holding the only slot.
func TestAResumedTurnTakesASlotUnderTheNodesCeiling(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	r.b.Node.MaxConcurrent = 1
	e := r.start(t)
	r.seedRun(t, e, "t1")
	fired := firedCompletions(e)

	_, release, err := e.node.AdmitTurn(t.Context(), "swe")
	if err != nil {
		t.Fatalf("AdmitTurn: %v", err)
	}
	t.Cleanup(release)
	r.runner.Finish(sandbox.Result{Success: true, Text: "fixed the flaky test"})
	select {
	case <-fired:
	case <-time.After(20 * time.Second):
		t.Fatal("the waiter never fired the finished job's completion")
	}
	eventually(t, "the completion to park for the node's one slot", func() bool {
		return e.node.TurnsWaiting() > 0 || r.collected.Load() > 0
	})
	if got := r.collected.Load(); got != 0 {
		t.Fatalf("the completion was collected %d times while the node's one slot was held", got)
	}
	if got := r.run(t, e, "t1"); got.Status != sandbox.StatusRunning {
		t.Fatalf("run = %q while the node was at its ceiling, want it unclaimed", got.Status)
	}

	release()
	eventually(t, "the completion to be collected once the slot freed", func() bool {
		return r.collected.Load() > 0
	})
}
