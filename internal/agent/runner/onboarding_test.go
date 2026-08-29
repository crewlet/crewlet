package runner_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// The onboarding guard chain, which is the part where a mistake is expensive
// in BOTH directions: a seat that re-onboards every turn burns its budget on
// reading pages it has read, and a seat that never onboards works for ever
// without knowing its team's conventions.

// markers is a marker store whose every answer a test controls.
type markers struct {
	onboarded bool
	readErr   error

	claims    int
	claimHeld bool
	claimErr  error
	released  int
}

func (m *markers) Onboarded(context.Context, string, string) (bool, error) {
	return m.onboarded, m.readErr
}

func (m *markers) Claim(_ context.Context, agentID string, _ time.Time, _ time.Duration) (learning.Pass, error) {
	m.claims++
	if m.claimErr != nil {
		return learning.Pass{}, m.claimErr
	}
	if !m.claimHeld {
		return learning.Pass{}, nil
	}
	return learning.Pass{AgentID: agentID}, nil
}

func (m *markers) Release(context.Context, learning.Pass, time.Time) error {
	m.released++
	return nil
}

// onboardingRunner builds a runner whose only registered tools are the two the
// pass needs, over a provider that always marks.
func onboardingRunner(t *testing.T, store runner.Markers, latch *runner.Latch, prov *scriptedProvider) *runner.Runner {
	t.Helper()
	reg := tools.NewRegistry()
	for _, name := range []string{"reflect_and_persist", "mark_onboarded"} {
		if err := reg.Register(stubTool{name: name, out: "ok"}, tools.OriginBuiltin); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: organization, Role: role},
		Registry: reg,
		Models:   models,
		Caps:     runner.Caps{PlanRounds: 2, ExecuteRounds: 2, ReviewRounds: 2},
		Onboarding: runner.Onboarding{
			Markers: store, Latch: latch, Rounds: 3, Ceiling: 0,
		},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

// marking is a provider that calls mark_onboarded on its first round.
func marking() *scriptedProvider {
	return &scriptedProvider{onboarding: []llm.Completion{
		{ToolCalls: []llm.ToolCall{{ID: "a", Name: "mark_onboarded",
			Arguments: map[string]any{"notes": "read the pages"}}}},
	}}
}

func TestAnUnmarkedSeatRunsThePassAndMarks(t *testing.T) {
	t.Parallel()
	store := &markers{claimHeld: true}
	r := onboardingRunner(t, store, runner.NewLatch(), marking())

	ran, err := r.Onboard(context.Background())
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if !ran {
		t.Fatal("the pass did not run for an unmarked seat")
	}
	if store.claims != 1 || store.released != 1 {
		t.Errorf("claims %d, releases %d — the lease must be taken and given "+
			"back, or a crashed pass blocks re-onboarding until the TTL",
			store.claims, store.released)
	}
}

func TestAMarkedSeatIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	store := &markers{onboarded: true, claimHeld: true}
	prov := marking()
	r := onboardingRunner(t, store, runner.NewLatch(), prov)

	if ran, err := r.Onboard(context.Background()); ran || err != nil {
		t.Errorf("ran = %v, err = %v", ran, err)
	}
	if store.claims != 0 {
		t.Error("a marked seat took the lease, which serialises every node's " +
			"turns behind a pass none of them will run")
	}
	if len(prov.requestsFor("onboarding")) != 0 {
		t.Error("a marked seat still ran an LLM pass")
	}
}

func TestAnUnreadableMarkerSkipsRatherThanReOnboards(t *testing.T) {
	t.Parallel()
	// THE TRI-STATE. Collapsing a failed lookup into "not onboarded" would
	// re-run a full pass for an already-marked agent on every transient
	// database blip — strictly worse than waiting one turn for the answer.
	store := &markers{readErr: errors.New("database is unreachable"), claimHeld: true}
	prov := marking()
	r := onboardingRunner(t, store, runner.NewLatch(), prov)

	ran, err := r.Onboard(context.Background())
	if err != nil {
		t.Fatalf("an unreadable marker failed the turn: %v", err)
	}
	if ran {
		t.Error("an unreadable marker re-ran the pass")
	}
	if store.claims != 0 || len(prov.requestsFor("onboarding")) != 0 {
		t.Error("the pass reached the lease or the model on an unknown state")
	}
}

func TestTheLatchSurvivesALaterReadFailure(t *testing.T) {
	t.Parallel()
	// The marker store is best-effort. Once THIS process has seen the seat
	// marked, no later read flake may re-fire a whole pass.
	latch := runner.NewLatch()
	store := &markers{onboarded: true, claimHeld: true}
	r := onboardingRunner(t, store, latch, marking())

	if ran, _ := r.Onboard(context.Background()); ran {
		t.Fatal("a marked seat ran the pass")
	}
	// Now the store falls over. The latch answers instead.
	store.readErr = errors.New("gone")
	store.onboarded = false
	if ran, _ := r.Onboard(context.Background()); ran {
		t.Error("a read failure after a confirmed marker re-ran the pass")
	}
}

func TestARestructureReOnboards(t *testing.T) {
	t.Parallel()
	// The latch is keyed by CHAIN HASH, which is what makes a live org
	// restructure re-onboard by design: a seat that moved under a different
	// unit is oriented to a company it has not read about.
	latch := runner.NewLatch()
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	before := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	after := &org.Organization{Name: "Acme Holdings", Roles: []*org.Role{role}}

	id, ok := before.AgentIDFor(role)
	if !ok {
		t.Fatal("no agent id")
	}
	latch.Confirm(id.String(), learning.ChainHash(before, role))

	if !latch.Confirmed(id.String(), learning.ChainHash(before, role)) {
		t.Error("the latch forgot the chain it was told about")
	}
	if latch.Confirmed(id.String(), learning.ChainHash(after, role)) {
		t.Error("the latch answered for a chain it never saw, so a restructured " +
			"seat would never re-onboard")
	}
}

func TestAPassClaimedElsewhereSkips(t *testing.T) {
	t.Parallel()
	// Seat inboxes are shared subscriptions, so during a rolling restart two
	// engines can each run a turn for the same un-onboarded seat. The lease
	// makes exactly one of them run the pass.
	store := &markers{claimHeld: false}
	prov := marking()
	r := onboardingRunner(t, store, runner.NewLatch(), prov)

	ran, err := r.Onboard(context.Background())
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if ran {
		t.Error("a seat whose pass another node claimed ran it too")
	}
	if len(prov.requestsFor("onboarding")) != 0 {
		t.Error("the loser of the lease still called the model")
	}
	if store.released != 0 {
		t.Error("the loser released a lease it never held")
	}
}

func TestAPassThatDoesNotMarkRetriesNextTurn(t *testing.T) {
	t.Parallel()
	// There is no rescue path, deliberately: a rescue would have to invent a
	// summary of pages the agent did not finish reading. An unmarked pass
	// just ends, and the next turn tries again.
	store := &markers{claimHeld: true}
	latch := runner.NewLatch()
	silent := &scriptedProvider{onboarding: []llm.Completion{text("I had a look around.")}}
	r := onboardingRunner(t, store, latch, silent)

	if ran, err := r.Onboard(context.Background()); !ran || err != nil {
		t.Fatalf("ran = %v, err = %v", ran, err)
	}
	// NOT latched: the seat is still unmarked, so the next turn must try.
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	id, _ := organization.AgentIDFor(role)
	if latch.Confirmed(id.String(), learning.ChainHash(organization, role)) {
		t.Error("an unmarked pass latched, so the seat never onboards again")
	}
}

func TestNoMarkerStoreMeansNoPass(t *testing.T) {
	t.Parallel()
	// Without somewhere to mark, the pass could never complete — it would
	// run every turn, for ever, spending a round budget each time.
	prov := marking()
	r := onboardingRunner(t, nil, runner.NewLatch(), prov)
	if ran, err := r.Onboard(context.Background()); ran || err != nil {
		t.Errorf("ran = %v, err = %v", ran, err)
	}
	if len(prov.requestsFor("onboarding")) != 0 {
		t.Error("a storeless node ran an onboarding pass")
	}
}

func TestAZeroBudgetDisablesThePass(t *testing.T) {
	t.Parallel()
	// turn_engine.onboarding_max_tool_rounds: 0 is the operator's off switch,
	// and it has to reach the pass before the lease — otherwise a company
	// that turned onboarding off still serialises its turns behind a claim.
	// Built with a zero budget rather than mutated: Config is per-turn.
	store := &markers{claimHeld: true}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "mark_onboarded"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	models, _ := phase.NewRegistry([]phase.Entry{{Key: "d", Provider: marking()}})
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	disabled, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models,
		Onboarding: runner.Onboarding{Markers: store, Latch: runner.NewLatch(), Rounds: 0},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if ran, _ := disabled.Onboard(context.Background()); ran {
		t.Error("a zero budget still ran the pass")
	}
	if store.claims != 0 {
		t.Error("a disabled pass took the lease")
	}
}

func TestTheOnboardingSurfaceCannotSubmitAPlan(t *testing.T) {
	t.Parallel()
	// The pass has its OWN budget precisely so it never competes with
	// planning; a surface that could submit a plan would make it a second
	// Plan phase with a different prompt.
	store := &markers{claimHeld: true}
	prov := marking()
	r := onboardingRunner(t, store, runner.NewLatch(), prov)
	if _, err := r.Onboard(context.Background()); err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	reqs := prov.requestsFor("onboarding")
	if len(reqs) == 0 {
		t.Fatal("the pass never called the model")
	}
	for _, def := range reqs[0].Tools {
		if def.Name == runner.SubmitPlanTool || def.Name == runner.SubmitReviewTool {
			t.Errorf("the onboarding surface offers %s", def.Name)
		}
	}
}
