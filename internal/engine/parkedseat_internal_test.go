package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
)

// A PROVIDER-PARKED SEAT WITH RESUMES WAITING ON IT STAYS QUIET, and resumes
// each of them once the park releases.
//
// Two answers the seat owes are both refused at the turn gate while every
// member their relaunch could take is at its limit: a coding CLI's entry that
// sits out a spent plan, in the catalogue this node launches on. A refusal
// there comes after the node admitted
// the turn, so the admission is released at once, and a release that woke the
// seat's other waiting resumes could set them waking each other for as long as
// the limit lasts. Each refused resume waits on the provider from then on, and
// the node announces a release only to a turn refused behind it
// (node.Config.SeatTurnEnded), so the gate is asked once per resume while the
// park holds, and once more each when it releases.
func TestAProviderParkedSeatWithWaitingResumesStaysQuiet(t *testing.T) {
	t.Parallel()
	r := newDrainRig(t)
	var asked atomic.Int32
	r.gate = func(next sandbox.TurnGate) sandbox.TurnGate {
		return func(ctx context.Context, handle string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			asked.Add(1)
			return next(ctx, handle, route)
		}
	}
	e := r.start(t)
	withLaunchModels(t, e, cliProvider(t, chainMember{key: "default", agent: "claude-code",
		model: "opus", spent: true}))
	stopWaiters(e)

	// TWO ANSWERS THE SEAT OWES, taken over as its holder recovers it.
	rt := e.sandbox.Load()
	answeredRun(t, e.answers, rt.pending, "a1")
	answeredRun(t, e.answers, rt.pending, "a2")
	host := e.node.Host()
	if !host.Release(t.Context(), "swe", seat.ReasonPlacement) {
		t.Fatal("the seat was not released")
	}
	host.Sweep(t.Context())
	waitHeld(t, e, "swe")
	eventually(t, "both answers to be refused at the gate", func() bool { return asked.Load() >= 2 })

	time.Sleep(300 * time.Millisecond)
	settled := asked.Load()
	time.Sleep(2 * time.Second)
	if more := asked.Load() - settled; more > 2 {
		t.Fatalf("the gate was asked %d more times in 2s while the park held: the seat's "+
			"waiting resumes woke each other", more)
	}

	// THE LIMIT LIFTS, as an apply that hands the seat an entry with a plan
	// to spend does: both are tried again, and claimed.
	withLaunchModels(t, e, cliProvider(t, chainMember{key: "default", agent: "claude-code",
		model: "opus"}))
	e.readmitResumes(waitProvider)
	eventually(t, "both answers to be claimed for their resumes", func() bool {
		for _, id := range []string{"a1", "a2"} {
			run, found, err := rt.pending.Get(t.Context(), id)
			if err != nil || (found && run.Status == sandbox.StatusAnswered && run.UpdatedAt.Equal(run.Answer.RecordedAt)) {
				return false
			}
		}
		return true
	})
}

// heldProbe is a model whose call is out until the case lets it go, as a probe's
// call is for the whole run of the entry's CLI: entered says a call is out.
type heldProbe struct {
	entered chan struct{}
	release chan struct{}
}

func newHeldProbe() *heldProbe {
	return &heldProbe{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (*heldProbe) Model() string { return "opus" }

func (m *heldProbe) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	select {
	case m.entered <- struct{}{}:
	default:
	}
	select {
	case <-m.release:
		return &llm.Completion{Content: "ok", Model: "opus"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// gatedSpyRuntime is [spyRuntime] with the engine's own turn gate in front of
// the resumes, counting how often it is asked: what a resume the park refuses
// meets, and nothing past it but the recorder.
func gatedSpyRuntime(t *testing.T, e *Engine) (*sandbox.Coordinator, *resumeSpy, *atomic.Int32) {
	t.Helper()
	rt := e.sandbox.Load()
	if rt == nil {
		t.Fatal("the node runs no sandbox")
	}
	resumer := &resumeSpy{}
	var asked atomic.Int32
	gate := e.turnGate()
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Pushes: noPushes{}, Audience: noAudience{}, Answers: e.answers, Queue: e.backends.Queue, Pending: rt.pending,
		Manager: rt.coordinator.Manager(), Resume: resumer,
		Hold: seatHold{engine: e}, Admit: e.mayResumeAnswer, Lease: e.launchFence,
		Gate: func(ctx context.Context, handle string, route sandbox.ResumeRoute) (context.Context, func(), error) {
			asked.Add(1)
			return gate(ctx, handle, route)
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e.sandbox.Store(&sandboxRuntime{pending: rt.pending, coordinator: coordinator})
	return coordinator, resumer, &asked
}

// A PARKED SEAT'S ANSWER WAITS FOR THE PROBE'S ANSWER, NOT AT AN INSTANT PAST.
//
// The seat is parked on its models' limit and owes a person's answer, which the
// turn gate refuses while the park stands. From the instant the chain's probe
// is due until its call comes back, a full run of the entry's CLI, that
// instant is past and the park still stands. The refusal named it, and the
// answer's retry re-checked at once, over and over: thousands of times a
// second for as long as the call was out, each re-reading the run, opening the
// answer's seal and taking the node's admission. What ends that wait is the
// probe's answer: the call comes back answered, the park is released, and the
// answer is resumed, once.
//
// Mutation: name the probe's instant in the park's refusal whether or not it is
// still to come (providerParkRefusal), and the gate is asked without end while
// the probe is due and its call is out.
func TestAParkedSeatsAnswerWaitsForItsProbesAnswer(t *testing.T) {
	t.Parallel()
	e := sandboxNode(t, nil)
	applyOK(t, e, sandboxWithModelDoc)
	waitHeld(t, e, "swe")
	alarms := &fakeAlarms{}
	e.providerParks.after = alarms.after
	model := newHeldProbe()
	reg, err := phase.NewRegistry([]phase.Entry{{Key: "zulu", Provider: model}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c := *e.Company()
	c.Models = reg
	e.epoch.current.Store(&c)

	// PARKED, ITS PROBE DUE NOW: the instant the alarm is armed for has come.
	refusal := &llm.Error{Kind: llm.KindRateLimit, Key: "zulu", RetryAt: time.Now(),
		Err: errors.New("You've hit your 5-hour limit")}
	limit, limited := chain.Limited(&chain.Error{Attempted: []string{"zulu"},
		Failures: []error{refusal}, Err: refusal})
	if !limited {
		t.Fatalf("the fixture's refusal is not a limit: %v", refusal)
	}
	if _, parked, err := e.providerPark(t.Context(), "swe", limit); err != nil || !parked {
		t.Fatalf("providerPark = %v, %v", parked, err)
	}

	coordinator, resumer, asked := gatedSpyRuntime(t, e)
	answeredRun(t, e.answers, e.sandbox.Load().pending, "t-parked")
	recoverHeld(t, e, coordinator, "swe")
	eventually(t, "the answer to be refused at the gate for the park", func() bool {
		return asked.Load() > 0
	})
	time.Sleep(500 * time.Millisecond)
	if n := asked.Load(); n > 2 {
		t.Fatalf("the gate was asked %d times in half a second while the probe was due", n)
	}

	// THE PROBE'S CALL IS OUT, as the alarm's goroutine runs it.
	live := alarms.live()
	if len(live) != 1 {
		t.Fatalf("%d probe alarms armed, want the park's", len(live))
	}
	go live[0].fire()
	select {
	case <-model.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe's call never went out")
	}
	before := asked.Load()
	time.Sleep(time.Second)
	if more := asked.Load() - before; more > 1 {
		t.Fatalf("the gate was asked %d more times in the second the probe's call was out, "+
			"want none: the answer re-checked at an instant already past", more)
	}
	if n := resumer.count(); n != 0 {
		t.Fatalf("%d resumes ran while the park stood", n)
	}

	// THE CALL ANSWERS: the park is released, and its release resumes the
	// answer, once.
	close(model.release)
	eventually(t, "the answer to resume once the park was released", func() bool {
		return resumer.count() > 0
	})
	time.Sleep(500 * time.Millisecond)
	if n := resumer.count(); n != 1 {
		t.Fatalf("the answer resumed %d times on the park's release, want once", n)
	}
}
