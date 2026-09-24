package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
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
			in.Turn = &turnctx.Turn{
				RunID: in.Run.TurnID, WorkKey: in.Run.WorkKey, Seat: seat, Org: company.Org,
			}

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

// THE ENGINE'S COORDINATOR READS THE BUDGET BEFORE IT CLAIMS. Built as a
// running node builds it, over a company at its cap, a finished run's
// completion is left unclaimed: the run stays running, so no box is collected
// and no turn is resumed into a budget that would refuse its first round.
func TestTheEnginesCoordinatorLeavesACompletionTheBudgetWouldStop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	company, seat := resumableCompany(t, unavailableModel{}, 1000)
	company.Config.Providers.Sandbox = &config.SandboxProvider{Fake: true}
	e := &Engine{backends: &Backends{Fleet: fleet, Queue: memory.New()}}
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
	}, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if ok, err := store.MarkSuspended(ctx, "t1", map[string]any{"version": 2}); err != nil || !ok {
		t.Fatalf("MarkSuspended = %v, %v", ok, err)
	}
	run, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	completion := types.SandboxRunCompleted{
		TurnID: "t1", LaunchID: run.LaunchID, AgentHandle: seat.Handle(), Agent: id.String(),
		RoleName: seat.Name,
	}
	if err := e.sandboxCoordinator.OnCompleted(ctx, completion,
		events.New(completion, events.TraceContext{})); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got, _, _ := store.Get(ctx, "t1"); got.Status != sandbox.StatusRunning {
		t.Errorf("status = %q, want the completion left unclaimed while the company is at its cap",
			got.Status)
	}
}
