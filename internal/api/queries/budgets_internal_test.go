package queries

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// A CALLER THIS NODE COULD NOT CHECK IS REFUSED, AS THE NODE'S CONDITION: the
// answer's `assistant` figures are the caller's own, and read as nobody's they
// would tell a person refused a question that they had spent nothing against
// the ceiling that refused them, for as long as identity cannot be read.
//
// THE HANDLER ITSELF, not the registry: the registry's posture refuses an
// unresolved caller before any answer runs, so a case through it passes with
// or without this arm, and the arm is what holds when a route reaches the
// handler some other way.
//
// Mutation: read the zero principal for an unknown resolution, and the
// handler answers for nobody.
func TestBudgetsRefuseACallerTheNodeCouldNotCheck(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n  sandbox: {fake: true}\n" +
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: p\nassistant:\n  token_budget: {day: 1000}\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	s := Sources{
		Company: func() (*config.Company, *org.Organization) { return cfg, roster },
		Budget:  coordmemory.NewFleet(),
		Now:     func() time.Time { return time.Date(2026, time.March, 14, 15, 0, 0, 0, time.UTC) },
	}
	ctx := iam.WithUnresolved(t.Context(), errors.New("the identity estate did not answer"))
	got, err := s.budgets(ctx, Params{})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("budgets for a caller nobody could check = (%v, %v), want ErrUnavailable", got, err)
	}
}
