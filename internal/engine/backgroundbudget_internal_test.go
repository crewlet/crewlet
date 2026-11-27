package engine

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// A SEAT AT ITS CEILING GETS NO BACKGROUND LEARNING CALL (#1838). The company
// set a token budget, and the docs promise it bounds learning spend; the
// clustered pass drafted a skill for a seat whose day was spent all the same,
// charging the call past the ceiling. max_share 1 takes the learning share out,
// so nothing but the budget's gate can hold the call. A seat with room still
// gets its call, so the gate holds the ceiling and not the pass.
//
// Red before the fix: the seat at its ceiling got its call.
func TestASeatAtItsCeilingGetsNoBackgroundLearningCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		spent int
		calls int
	}{
		{"at its ceiling", 100, 0},
		{"with room", 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			c := learningCompany(t, "  max_share: 1\n  skill_synthesis:\n    scheduler_enabled: true\n")
			seat := c.Org.Role("eng")
			if seat == nil {
				t.Fatal("the company has no seat eng")
			}
			seat.TokenBudget = org.TokenCeilings{period.Day: 100}
			// The seat's chain names gateway; its one entry answers and counts.
			provider := &answeringProvider{in: 10, out: 10}
			registry, err := phase.NewRegistry([]phase.Entry{{Key: "gateway", Provider: provider}})
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			c.Models = registry
			fleet := coordmem.NewFleet()
			e := engineOver(t)
			e.backends.Fleet, e.clock = fleet, fixedClock(budgetNow)

			windows := coord.WindowsAt(budgetNow, time.UTC)
			if _, err = fleet.PostCharge(ctx, scopeOf(t, c, seat), tc.spent, windows); err != nil {
				t.Fatalf("PostCharge: %v", err)
			}
			episodes := learning.NewEpisodes(e.backends.Store)
			base := time.Now().UTC().Add(-6 * time.Hour) // inside the pass's window, which it reads from the wall
			for i := range 4 {
				if _, err = episodes.Append(ctx, learning.Episode{
					ID: fmt.Sprintf("ep-%d", i), Handle: "eng", Role: seat.Name, TurnID: fmt.Sprintf("turn-%d", i),
					StartedAt: base.Add(time.Duration(i) * time.Hour), EndedAt: base.Add(time.Duration(i)*time.Hour + time.Minute),
					TaskSummary: "ship release", PlanSummary: "cut, tag, announce",
					ToolSequence: []string{"fetch", "build", "tag", "announce", "publish"}, ReviewOutcome: "done",
					Kind: learning.KindRaw,
				}); err != nil {
					t.Fatalf("Append: %v", err)
				}
			}
			pass := e.clusteringPass(c)
			if pass == nil {
				t.Fatal("no clustering pass was built")
			}
			id, ok := c.Org.AgentIDFor(seat)
			if !ok {
				t.Fatal("eng has no agent id")
			}
			// The draft's answer does not matter here, only whether it was asked.
			_, _ = pass.ClusterPass(ctx, seat, id.String())
			if provider.calls != tc.calls {
				t.Fatalf("model calls = %d with %d of the seat's 100 spent, want %d", provider.calls, tc.spent, tc.calls)
			}
		})
	}
}
