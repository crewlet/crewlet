package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/period"
)

// assistantCompany is a company capping its own day at org and each person's
// assistant day at person.
func assistantCompany(org, person int) *Company {
	c := meteredCompany(config.TokenBudget{Day: ceiling(org)})
	c.Config.Assistant.TokenBudget = config.TokenBudget{Day: ceiling(person)}
	return c
}

func assistantEngine(t *testing.T, c *Company) (*Engine, *coordmem.Fleet) {
	t.Helper()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}, clock: fixedClock(budgetNow),
		refusals: newRefusalLatch()}
	e.epoch.current.Store(c)
	return e, fleet
}

func personNamed(name string) iam.Principal {
	return iam.Principal{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)), Seat: name}
}

// A PERSON'S DAY BUDGET STOPS THEIR TURNS: once the person has spent their own
// assistant day (`assistant.token_budget`), their next turn is refused before
// its first model round, naming their own scope, while another person's turns
// still run.
//
// Mutation: meter every person on the company alone (drop the personCaps
// branch in assistantBudget.Meter) and grace's next turn runs.
func TestAPersonsDayBudgetStopsTheirTurns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, fleet := assistantEngine(t, assistantCompany(1_000_000, 500))
	budget := AssistantBudget(e)
	grace, ada := personNamed("grace"), personNamed("ada")

	m, err := budget.Meter(ctx, grace)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.Spend(ctx, 500); err != nil || !got.OK {
		t.Fatalf("grace's round inside her day = (%+v, %v), want admitted", got, err)
	}
	next, err := budget.Meter(ctx, grace)
	if err != nil {
		t.Fatal(err)
	}
	var refused *toolloop.BudgetError
	if err := toolloop.Refusal(ctx, next); !errors.As(err, &refused) {
		t.Fatalf("grace's next turn before its first round: %v, want her budget's refusal", err)
	}
	if refused.Scope != assistant.PersonBudgetScope || refused.Period != period.Day {
		t.Fatalf("the refusal names %s's %s, want the person's own day", refused.Scope,
			refused.Period)
	}
	windows := coord.WindowsAt(budgetNow, time.UTC)
	if spent := spentIn(t, fleet, coord.PersonScope(grace.ID), windows, period.Day); spent != 500 {
		t.Errorf("grace's own counter holds %d, want the 500 she spent", spent)
	}
	if spent := spentIn(t, fleet, coord.OrgScope, windows, period.Day); spent != 500 {
		t.Errorf("the company's counter holds %d, want grace's 500 on it too", spent)
	}
	other, err := budget.Meter(ctx, ada)
	if err != nil {
		t.Fatal(err)
	}
	if err := toolloop.Refusal(ctx, other); err != nil {
		t.Fatalf("ada's turn was refused on grace's spend: %v", err)
	}
}

// A RAISED ASSISTANT CEILING TAKES EFFECT ON THE NEXT TURN, WITH NO RESTART.
// The remedy a person's refusal carries opens the Budgets screen, whose save
// is a company revision this node applies; the meter reads the ceilings off the
// company it runs NOW, per turn, so the person refused a moment ago asks again
// and is admitted as soon as the revision is applied.
//
// Mutation: read the ceilings once, when the meter's engine starts, and the
// raised ceiling still refuses grace.
func TestARaisedAssistantCeilingAdmitsTheNextTurn(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, _ := assistantEngine(t, assistantCompany(1_000_000, 500))
	budget := AssistantBudget(e)
	grace := personNamed("grace")
	m, err := budget.Meter(ctx, grace)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.Spend(ctx, 500); err != nil || !got.OK {
		t.Fatalf("grace's round inside her day = (%+v, %v), want admitted", got, err)
	}
	spent, err := budget.Meter(ctx, grace)
	if err != nil {
		t.Fatal(err)
	}
	if err := toolloop.Refusal(ctx, spent); err == nil {
		t.Fatal("grace's next turn ran on a spent day, so this case proves nothing")
	}
	// THE APPLIED REVISION: the same company with the person's day raised.
	e.epoch.current.Store(assistantCompany(1_000_000, 2_000))
	raised, err := budget.Meter(ctx, grace)
	if err != nil {
		t.Fatal(err)
	}
	if err := toolloop.Refusal(ctx, raised); err != nil {
		t.Errorf("grace's turn after the ceiling was raised: %v, want admitted", err)
	}
}

// THE COMPANY'S WINDOWS STILL HOLD A CAPPED PERSON: a person with room left
// under their own ceiling is refused once the company's day is spent, and the
// refusal names the company.
//
// Mutation: charge a capped person to their own scope alone (OrgCaps nil in
// the person's meter) and grace's turn runs on a spent company.
func TestTheCompanysWindowsStillHoldACappedPerson(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, _ := assistantEngine(t, assistantCompany(600, 500))
	budget := AssistantBudget(e)
	grace, ada := personNamed("grace"), personNamed("ada")
	for _, spend := range []struct {
		p      iam.Principal
		tokens int
	}{{grace, 300}, {ada, 300}} {
		m, err := budget.Meter(ctx, spend.p)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := m.Spend(ctx, spend.tokens); err != nil || !got.OK {
			t.Fatalf("%s's round = (%+v, %v), want admitted", spend.p.Seat, got, err)
		}
	}
	next, err := budget.Meter(ctx, grace)
	if err != nil {
		t.Fatal(err)
	}
	var refused *toolloop.BudgetError
	if err := toolloop.Refusal(ctx, next); !errors.As(err, &refused) || refused.Scope != "org" {
		t.Fatalf("grace's turn on a spent company: %v, want the company's refusal", err)
	}
}

// A NODE IS GONE ONLY WHEN ITS PRESENCE LEASE IS: the assistant closes a turn
// interrupted on the roster's word, so a store that cannot answer must never
// read as an absent node.
//
// Mutation: answer (false, nil) on a read error and an unreachable store
// closes every remote turn interrupted.
func TestTheRosterNeverReadsAnUnreachableStoreAsGone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	backend := coordmem.New()
	roster := nodeRoster{coord: backend}
	if lease, _, err := backend.TryAcquire(ctx, coord.NodeResource("node-a"),
		coord.AcquireOptions{Owner: "node-a/1", TTL: time.Minute}); err != nil || lease == nil {
		t.Fatalf("acquire node-a's presence: %v", err)
	}
	if ok, err := roster.Present(ctx, "node-a"); err != nil || !ok {
		t.Fatalf("a node holding its lease reads %v, %v; want present", ok, err)
	}
	if ok, err := roster.Present(ctx, "node-b"); err != nil || ok {
		t.Fatalf("a node holding no lease reads %v, %v; want absent", ok, err)
	}
	if nodes, err := roster.Nodes(ctx); err != nil || len(nodes) != 1 || nodes[0] != "node-a" {
		t.Fatalf("the roster lists %v, %v; want node-a, the one node holding its lease", nodes, err)
	}
	down := nodeRoster{coord: unreachable{backend}}
	if ok, err := down.Present(ctx, "node-b"); err == nil {
		t.Fatalf("an unreachable store reads %v with no error; want the error", ok)
	}
}

// unreachable is a coordination store that cannot be read.
type unreachable struct{ coord.Backend }

func (unreachable) Get(context.Context, string) (*coord.Lease, error) {
	return nil, errors.New("the store is away")
}
