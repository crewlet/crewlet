package worklet

import (
	"errors"
	"fmt"
	"slices"
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
		"exactly the cap, still enumerated":  {reach: reachInENG(MaxScopeTerms - 1), terms: MaxScopeTerms},
		"one over the cap, its project wide": {reach: reachInENG(MaxScopeTerms), terms: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			scope := purgeScope("purged", "ENG", tc.reach, nil)
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

// reachInENG is n tasks in the purged task's own project.
func reachInENG(n int) purgeReach {
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
	named := purgeScope("purged", "ENG", purgeReach{"a": "ENG"}, nil)
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
			scope: purgeScope("purged", "ENG", nil, nil), reach: purgeReach{"a": "ENG"}, short: "a",
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

// A PURGE'S SCOPE NAMES EVERY KEY IT HANDS ON, EACH UNDER ITS OWN PROJECT.
//
// The apply releases the purged task's claim on its current key and on every
// former one and hands each to a task still holding it, which is decided by
// the records filed under that key — a create of a duplicate, a move onto it.
// A scope naming the task alone let the purge overtake such a record on the
// node that retained it, and hand the key to another task there than on
// every other node. A former key sits under the project that minted it, which
// a move has left, so the purged task's own project does not cover it; and
// under the cap the keys fold with the tasks into their project's container.
func TestAPurgesScopeNamesEveryKeyItHandsOn(t *testing.T) {
	t.Parallel()
	keys := []string{"OPS-3", "ENG-7", "WEB-2"}
	scope := purgeScope("purged", "OPS", nil, keys)
	if err := scope.Validate(); err != nil {
		t.Fatalf("the purge's scope is refused: %v", err)
	}
	if err := scope.coversKeys(keys); err != nil {
		t.Fatalf("the purge's scope does not cover the keys it hands on: %v", err)
	}
	resolved := scope.Resolve(TaskSubject("purged"))
	for _, key := range keys {
		project, _, _ := strings.Cut(key, "-")
		path := statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermKey, Container: project, ID: key}.Path(),
		}}
		if !resolved.Intersects(path) {
			t.Errorf("a record deferred under key %s does not meet the purge's "+
				"scope %v", key, resolved.Paths)
		}
	}
	if self := scope.coversReach(purgeReach{"purged": "OPS"}); self != nil {
		t.Errorf("the scope does not name the purged task itself: %v", self)
	}

	// UNDER THE CAP, the keys fold with the tasks of their project.
	epic := reachInENG(MaxScopeTerms)
	folded := purgeScope("purged", "ENG", epic, []string{"ENG-7", "OPS-1"})
	if err := folded.Validate(); err != nil {
		t.Fatalf("the folded scope is refused: %v", err)
	}
	if err := folded.coversKeys([]string{"ENG-7", "OPS-1"}); err != nil {
		t.Errorf("the folded scope does not cover the keys: %v", err)
	}
	if err := folded.coversReach(epic); err != nil {
		t.Errorf("the folded scope does not cover the reach: %v", err)
	}
	if got := len(folded.Terms); got != 2 {
		t.Errorf("the folded scope has %d terms, want ENG's container and OPS-1's "+
			"key: %+v", got, folded.Terms)
	}
}

// A SCOPE SHORT OF A KEY THE WRITE MOVES IS REFUSED NAMING THE KEY.
//
// The keys are read before the request, so a task re-keyed in between holds
// one the request never probed; the decide refuses rather than publishing a
// record filed where no deferral under that key is looked for.
func TestAScopeShortOfAKeyIsRefusedNamingTheKey(t *testing.T) {
	t.Parallel()
	named := purgeScope("purged", "ENG", nil, []string{"ENG-7"})
	for name, tc := range map[string]struct {
		scope ScopeSet
		keys  []string
		short string
	}{
		"a former key the scope never read": {
			scope: named, keys: []string{"ENG-7", "OPS-2"}, short: "OPS-2",
		},
		"the task alone": {
			scope: purgeScope("purged", "ENG", nil, nil), keys: []string{"ENG-7"},
			short: "ENG-7",
		},
		"another project's container": {
			scope: ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: "OPS"}}},
			keys:  []string{"ENG-7"}, short: "ENG-7",
		},
		"the key under another project": {
			scope: ScopeSet{Terms: []ScopeTerm{
				{Kind: TermKey, Container: "OPS", ID: "ENG-7"},
			}},
			keys: []string{"ENG-7"}, short: "ENG-7",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.scope.coversKeys(tc.keys)
			if !errors.Is(err, statelog.ErrConflict) ||
				!strings.Contains(err.Error(), "key "+tc.short+" ") {
				t.Fatalf("coversKeys = %v, want a conflict naming key %s", err, tc.short)
			}
		})
	}
	for _, wide := range []ScopeSet{
		{Terms: []ScopeTerm{{Kind: TermDomain}}},
		{Terms: []ScopeTerm{{Kind: TermContainer, ID: "ENG"}}},
	} {
		if err := wide.coversKeys([]string{"ENG-7"}); err != nil {
			t.Errorf("%+v does not cover ENG-7: %v", wide.Terms, err)
		}
	}
}

// AN ENUMERATION PAST THE CAP FOLDS, NEVER FAILS.
//
// Each widening a writer makes is bounded on its own — sixty-four dependents,
// a task's keys, a purge's reach — and their sum is not: a blocker somebody
// filled has sixty-four dependents and its own term besides, and the scope
// [ScopeSet.Validate] refused was refused inside the decide of every write to
// it, for ever. The fold is the cap's own rule: the largest container first,
// the domain when the containers alone are too many.
func TestAnEnumerationPastTheCapFolds(t *testing.T) {
	t.Parallel()
	full := ScopeSet{Subject: true, Container: "ENG"}
	ids := make([]string, 0, MaxDependents)
	filed := map[string]string{}
	for i := range MaxDependents {
		id := fmt.Sprintf("dep-%02d", i)
		ids = append(ids, id)
		filed[id] = "ENG"
		if i%8 == 0 {
			filed[id] = "OPS"
		}
	}
	scope := full.withObjects("blocker", ids, filed).capped()
	if err := scope.Validate(); err != nil {
		t.Fatalf("a blocker with %d dependents states a scope the cap refuses: %v",
			MaxDependents, err)
	}
	if err := scope.covers(ids, filed); err != nil {
		t.Fatalf("the folded scope does not cover the dependents: %v", err)
	}
	if !slices.Contains(scope.Terms, ScopeTerm{Kind: TermContainer, ID: "ENG"}) {
		t.Errorf("the largest project was not the one folded: %+v", scope.Terms)
	}

	spread := ScopeSet{Terms: []ScopeTerm{}}
	for i := range MaxScopeTerms + 1 {
		spread.Terms = append(spread.Terms, ScopeTerm{
			Kind: TermObject, Container: fmt.Sprintf("P%d", i), ID: "t",
		})
	}
	if got := spread.capped(); len(got.Terms) != 1 || got.Terms[0].Kind != TermDomain {
		t.Errorf("an enumeration over more containers than the cap folded to %+v, "+
			"want the domain", got.Terms)
	}

	under := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermObject, Container: "ENG", ID: "a"},
		{Kind: TermObject, Container: "ENG", ID: "a"},
		{Kind: TermContainer, ID: "OPS"},
		{Kind: TermObject, Container: "OPS", ID: "b"},
	}}
	if got := under.capped(); len(got.Terms) != 2 {
		t.Errorf("a scope under the cap kept a duplicate or a term its container "+
			"already covers: %+v", got.Terms)
	}
}
