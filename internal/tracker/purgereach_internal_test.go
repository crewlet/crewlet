package tracker

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A PURGE'S SCOPE NAMES ITS WHOLE REACH AND STAYS UNDER THE TERM CAP.
//
// The cap is the design's universal batch, and a writer whose reach is larger
// states the smallest covering term instead — a purged epic's hundred children
// are one project's container, not a refused purge and not a scope the
// framework rejects. A reach spread over more projects than the cap, or naming
// a task this node holds no row of, is the domain: the one term that certainly
// covers it.
func TestAPurgesScopeNamesItsWholeReachUnderTheCap(t *testing.T) {
	t.Parallel()
	epic := purgeReach{}
	for i := range 100 {
		epic[fmt.Sprintf("child-%03d", i)] = "ENG"
	}
	for i := range 3 {
		epic[fmt.Sprintf("waits-%d", i)] = "OPS"
	}
	spread := purgeReach{}
	for i := range MaxScopeTerms + 1 {
		spread[fmt.Sprintf("t-%d", i)] = fmt.Sprintf("P%d", i)
	}
	for name, tc := range map[string]struct {
		reach purgeReach
		terms int
	}{
		"a task nothing else names":          {reach: purgeReach{}, terms: 0},
		"a few tasks, each named":            {reach: purgeReach{"a": "ENG", "b": "OPS"}, terms: 3},
		"an epic's children, by container":   {reach: epic, terms: 1 + 3},
		"more projects than the cap":         {reach: spread, terms: 1},
		"a task this node holds no row of":   {reach: purgeReach{"a": "ENG", "gone": ""}, terms: 1},
		"exactly the cap, still enumerated":  {reach: capped(MaxScopeTerms - 1), terms: MaxScopeTerms},
		"one over the cap, its project wide": {reach: capped(MaxScopeTerms), terms: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scope := purgeScope("purged", "ENG", tc.reach)
			if err := scope.Validate(); err != nil {
				t.Fatalf("the purge's scope is refused: %v", err)
			}
			if err := scope.coversReach(tc.reach); err != nil {
				t.Fatalf("the purge's scope does not cover its reach: %v", err)
			}
			if got := len(scope.Terms); got != tc.terms {
				t.Errorf("the scope has %d terms, want %d: %+v", got, tc.terms, scope.Terms)
			}
			// THE PURGED TASK ITSELF, which a scope built from terms
			// carries no sentinel for.
			self := scope.coversReach(purgeReach{"purged": "ENG"})
			if self != nil && !scope.Subject {
				t.Errorf("the scope does not name the purged task itself: %v", self)
			}
		})
	}
}

// capped is n tasks in the purged task's own project.
func capped(n int) purgeReach {
	reach := purgeReach{}
	for i := range n {
		reach[fmt.Sprintf("t-%02d", i)] = "ENG"
	}
	return reach
}

// A SCOPE THAT DOES NOT NAME A TASK IN THE REACH, OR NAMES IT WHERE IT IS NOT
// FILED, IS REFUSED NAMING THE TASK.
//
// The request's scope is what the publisher probed the deferral index with, so
// a decide that found a task the scope misses must refuse rather than widen it:
// a record deferred under that task was never asked about.
func TestAScopeShortOfThePurgesReachIsRefusedNamingTheTask(t *testing.T) {
	t.Parallel()
	named := purgeScope("purged", "ENG", purgeReach{"a": "ENG"})
	for name, tc := range map[string]struct {
		scope ScopeSet
		reach purgeReach
		short string
	}{
		"a task the scope never read": {
			scope: named, reach: purgeReach{"a": "ENG", "b": "ENG"}, short: "b",
		},
		"a task that moved project since": {
			scope: named, reach: purgeReach{"a": "OPS"}, short: "a",
		},
		"a container for another project": {
			scope: ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: "OPS"}}},
			reach: purgeReach{"a": "ENG"}, short: "a",
		},
		"a task this node holds no row of, under a container": {
			scope: ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: "ENG"}}},
			reach: purgeReach{"gone": ""}, short: "gone",
		},
		"the purged task alone, which a reach has outgrown": {
			scope: purgeScope("purged", "ENG", nil), reach: purgeReach{"a": "ENG"}, short: "a",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.scope.coversReach(tc.reach)
			if !errors.Is(err, statelog.ErrConflict) ||
				!strings.Contains(err.Error(), "task "+tc.short+" ") {
				t.Fatalf("coversReach = %v, want a conflict naming task %s", err, tc.short)
			}
		})
	}
	domain := ScopeSet{Terms: []ScopeTerm{{Kind: TermDomain}}}
	if err := domain.coversReach(purgeReach{"a": "ENG", "gone": ""}); err != nil {
		t.Errorf("the domain does not cover a reach: %v", err)
	}
}
