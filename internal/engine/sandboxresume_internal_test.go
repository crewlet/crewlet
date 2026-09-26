package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// unavailableModel fails every call with a server error: the transient failure
// a resumed turn is handed back for a retry on.
type unavailableModel struct{}

func (unavailableModel) Model() string { return "unavailable" }

func (unavailableModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, &llm.Error{Kind: llm.KindServer, Provider: "test", Model: "unavailable",
		Status: 503, Err: errors.New("service unavailable")}
}

// resumableCompany is one seat on one native model, with the company-wide token
// budget given.
func resumableCompany(t *testing.T, provider llm.Provider, orgBudget int) (*Company, *org.Role) {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: provider}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	seat := &org.Role{Name: "SWE", DeclaredHandle: "swe", LLM: org.ProviderKeys{"default"}}
	return &Company{
		Models: models, Tools: tools.NewRegistry(),
		Org:    &org.Organization{Name: "Acme", Roles: []*org.Role{seat}},
		Config: &config.Company{TokenBudget: orgBudget},
	}, seat
}

// THE COORDINATOR'S BUDGET READ IS THE SEAT'S OWN METER, under the epoch live
// now: a resume would run under that epoch, so a cap raised since the run
// detached is the cap its wait ends on.
func TestAResumesRoomIsReadOffTheSeatsMeter(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	company, seat := resumableCompany(t, unavailableModel{}, 1000)
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.epoch.current.Store(company)
	id, ok := company.Org.AgentIDFor(seat)
	if !ok {
		t.Fatal("the seat has no agent id")
	}
	if _, err := fleet.PostCharge(t.Context(), coord.AgentScope(id.String()), 1000); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}

	room, err := resumeRoom{engine: e}.Room(t.Context(), seat.Handle())
	if err != nil {
		t.Fatalf("Room: %v", err)
	}
	if room.OK || room.Scope != "org" || room.Used != 1000 || room.Limit != 1000 {
		t.Errorf("room = %+v, want the company's cap reached at 1000/1000", room)
	}

	// The cap raised by the revision now live.
	raised, _ := resumableCompany(t, unavailableModel{}, 5000)
	e.epoch.current.Store(raised)
	room, err = resumeRoom{engine: e}.Room(t.Context(), seat.Handle())
	if err != nil || !room.OK {
		t.Errorf("room after the cap was raised = %+v, %v, want room", room, err)
	}

	// No ceiling anywhere is no meter, and always room.
	uncapped, _ := resumableCompany(t, unavailableModel{}, 0)
	e.epoch.current.Store(uncapped)
	if room, err := (resumeRoom{engine: e}).Room(t.Context(), seat.Handle()); err != nil || !room.OK {
		t.Errorf("room with no budget = %+v, %v, want room", room, err)
	}
}

// A RESUME HANDED BACK SAYS WHETHER ITS RECORD COUNTED THE CARRIED SPEND, and
// the coordinator writes that onto the run's row with the release, so the
// retry counts only what it bills. A resume told the spend was already counted
// has nothing to say.
func TestAResumeHandedBackSaysWhetherItCountedTheCarriedSpend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		counted bool
		want    bool
	}{
		{"nothing had counted it", false, true},
		{"an earlier attempt's record counted it", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			company, seat := resumableCompany(t, unavailableModel{}, 0)
			e, _ := resumingEngine(t)
			e.backends = &Backends{Queue: memory.New()}
			e.epoch.current.Store(company)
			in := resumed("slack:C1")
			in.Company = company
			in.Run.AgentHandle = seat.Handle()
			in.Run.CarriedCounted = tc.counted

			err := e.resumeTurn(t.Context(), in)
			if err == nil || errors.Is(err, sandbox.ErrResumeAbandoned) {
				t.Fatalf("resumeTurn = %v, want a failure handed back for a retry", err)
			}
			if got := errors.Is(err, sandbox.ErrCarriedCounted); got != tc.want {
				t.Errorf("the error carries the counted mark = %v, want %v: %v", got, tc.want, err)
			}
		})
	}
}

// budgetedSandboxEngine is a node's sandbox runtime as a running node builds
// it, over one seat whose company is at its 1000-token cap, with a run parked
// on a question on chat:C1.
func budgetedSandboxEngine(t *testing.T) (*Engine, *org.Role, *memory.Queue) {
	t.Helper()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	company, seat := resumableCompany(t, unavailableModel{}, 1000)
	company.Config.Providers.Sandbox = &config.SandboxProvider{Fake: true}
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	e := &Engine{backends: &Backends{Fleet: fleet, Queue: q}}
	e.epoch.current.Store(company)
	if err := e.buildSandboxRuntime(company); err != nil {
		t.Fatalf("buildSandboxRuntime: %v", err)
	}
	id, _ := company.Org.AgentIDFor(seat)
	if _, err := fleet.PostCharge(ctx, coord.AgentScope(id.String()), 1000); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}

	store := e.sandboxPending
	if err := store.BeginLaunch(ctx, sandbox.PendingRun{
		TurnID: "t1", AgentHandle: seat.Handle(), AgentID: id.String(), Role: seat.Name,
		ConversationKey: "chat:C1",
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if ok, err := store.MarkSuspended(ctx, "t1", map[string]any{"version": 2}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	if err := store.MarkAwaiting(ctx, "t1", sandbox.Clarification{Question: "which branch?"}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	return e, seat, q
}

// THE ENGINE'S COORDINATOR READS THE BUDGET BEFORE IT RESUMES. Built as a
// running node builds it, over a company at its cap, a person's reply to a
// parked run is held on the run's row rather than handed to a turn whose first
// round would be refused before it was sent.
func TestTheEnginesCoordinatorHoldsAnAnswerTheBudgetWouldStop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, seat, _ := budgetedSandboxEngine(t)

	handled, err := e.sandboxCoordinator.TryResumeFromAnswer(ctx, seat.Handle(), "chat:C1", "use main", nil)
	if err != nil || !handled {
		t.Fatalf("TryResumeFromAnswer = %v, %v, want the reply held and handled", handled, err)
	}
	run, _, err := e.sandboxPending.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if held, ok := run.Held(); !ok || held.Text != "use main" || run.Status != sandbox.StatusAwaiting {
		t.Errorf("the run reads status=%q held=%+v (%v), want the reply held on the parked run",
			run.Status, held, ok)
	}
}

// THE ENGINE'S WAITER READS THE SAME BUDGET before it signals a held answer:
// with the company at its cap a tick signals nothing, and once the cap is
// raised the next tick signals the seat's node.
func TestTheEnginesWaiterSignalsAHeldAnswerOnlyWithRoom(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, seat, q := budgetedSandboxEngine(t)
	if handled, err := e.sandboxCoordinator.TryResumeFromAnswer(ctx, seat.Handle(), "chat:C1",
		"use main", nil); err != nil || !handled {
		t.Fatalf("TryResumeFromAnswer = %v, %v", handled, err)
	}
	// An interval no test outlives, so the loop the waiter starts never
	// ticks on its own and every tick here is the test's.
	if err := e.startSandboxWaiter(ctx, time.Hour); err != nil {
		t.Fatalf("startSandboxWaiter: %v", err)
	}
	t.Cleanup(e.stopSandbox)
	signals := func() int {
		n := 0
		for _, ev := range q.History() {
			if _, ok := ev.Data.(*types.SandboxAnswerReady); ok {
				n++
			}
		}
		return n
	}

	if _, err := e.sandboxWaiter.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := signals(); n != 0 {
		t.Fatalf("the waiter signalled a held answer %d times with the company at its cap", n)
	}
	raised, _ := resumableCompany(t, unavailableModel{}, 5000)
	e.epoch.current.Store(raised)
	if _, err := e.sandboxWaiter.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := signals(); n != 1 {
		t.Errorf("the waiter signalled the held answer %d times once the cap was raised, want once", n)
	}
}

// submitsThenReviewFails answers the executor with a submission and fails
// every review with a server error, the transient kind a resume is handed back
// for.
type submitsThenReviewFails struct{}

func (submitsThenReviewFails) Model() string { return "submits-then-review-fails" }

func (submitsThenReviewFails) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	for _, def := range req.Tools {
		if def.Name == runner.SubmitReviewTool {
			return unavailableModel{}.Complete(ctx, req)
		}
	}
	return &llm.Completion{ToolCalls: []llm.ToolCall{{ID: "s", Name: runner.SubmitWorkTool,
		Arguments: map[string]any{"outcome": "blocked", "summary": "waiting on access",
			"evidence": "no write tool yet"}}}}, nil
}

// A RESUME WHOSE RE-ENTERED ROUND FINISHED AND WHOSE REVIEW BROKE SAYS ITS
// RECORD COUNTED THE CARRIED SPEND.
//
// The re-entered round published the record that counts what the phase billed
// before it suspended — the finished pass's record, not a failure's — and then
// the review broke and the turn is handed back. The retry's records must count
// only what it bills, which the coordinator writes onto the run's row from this
// error. For a native resume and an agent-mode one alike.
func TestAResumeThatFinishedAndWhoseReviewBrokeSaysItCountedTheCarriedSpend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		agent bool
	}{
		{"a native resume", false},
		{"an agent-mode resume", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			company, seat := resumableCompany(t, submitsThenReviewFails{}, 0)
			// The shipped caps, so the re-entered round has rounds to finish
			// in and its review is reached.
			company.Config.TurnEngine = config.DefaultTurnEngine()
			q := memory.New()
			if err := q.Start(ctx); err != nil {
				t.Fatalf("Start: %v", err)
			}
			e, _ := resumingEngine(t)
			e.backends = &Backends{Queue: q}
			e.epoch.current.Store(company)
			in := resumed("slack:C1")
			in.Company = company
			in.Run.AgentHandle = seat.Handle()
			in.Run.Reply = "tool"
			if tc.agent {
				store := sandbox.NewCoordStore(coordmemory.NewFleet())
				if err := store.BeginLaunch(ctx, sandbox.PendingRun{
					TurnID: in.Run.TurnID, AgentHandle: seat.Handle(),
				}, sandbox.Fence{}); err != nil {
					t.Fatalf("BeginLaunch: %v", err)
				}
				if ok, err := store.AppendBridgeCall(ctx, in.Run.TurnID, sandbox.BridgeCall{
					Name: runner.SubmitWorkTool,
					Args: `{"outcome":"blocked","summary":"waiting on access","evidence":"no write tool yet"}`,
				}); err != nil || !ok {
					t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
				}
				run, _, err := store.Get(ctx, in.Run.TurnID)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				run.Reply = "tool"
				in.Run = run
				e.sandboxPending = store
				in.State = execstate.State{Version: execstate.Version, AgentRun: true, Round: 1}
			}

			err := e.resumeTurn(ctx, in)
			if err == nil || errors.Is(err, sandbox.ErrResumeAbandoned) {
				t.Fatalf("resumeTurn = %v, want the review's failure handed back for a retry", err)
			}
			if !strings.Contains(err.Error(), "review") {
				t.Fatalf("resumeTurn = %v: the premise is a re-entered round that finished and a "+
					"review that broke", err)
			}
			if !errors.Is(err, sandbox.ErrCarriedCounted) {
				t.Errorf("the error does not say the finished round's record counted the carried "+
					"spend, so the retry counts it again: %v", err)
			}
		})
	}
}
