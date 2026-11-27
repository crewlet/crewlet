package engine

import (
	"fmt"
	"strings"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
)

// AN ADVISOR PASS WHOSE COMPANY BUDGET CANNOT BE READ DOES NOT START (#1838).
// A full window the tool loop refuses before the first round; an unreadable
// one it passed over, running the pass against a ceiling nobody could read.
// A readable budget starts it, and a company that caps nothing never asks.
//
// Mutation: drop the pre-flight, and the pass starts on the unreadable store.
func TestAnAdvisorPassOnAnUnreadableBudgetDoesNotStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	company := func(budget string) *Company {
		t.Helper()
		c, err := NewCompany(parseCompany(t, fmt.Sprintf(`name: Acme
%sproviders:
  llm:
    sonnet:
      harness: claude-code
      model: sonnet
      state_dir: %s/sonnet
advisor: {model: sonnet}
`, budget, dir)))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	over := func(c *Company, unreadable bool) *Engine {
		fleet, _ := counted(coordmem.NewFleet(), nil)
		e := &Engine{backends: &Backends{Fleet: fleet}}
		if unreadable {
			e.backends.Fleet = unreadableCounted{fleet}
		}
		e.epoch.current.Store(c)
		return e
	}
	capped := company("token_budget: {day: 100}\n")
	if _, err := AdvisorModels(over(capped, false)).Deps(t.Context(), nil); err != nil {
		t.Fatalf("a pass over a readable budget did not start: %v", err)
	}
	_, err := AdvisorModels(over(capped, true)).Deps(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "token budget could not be read") {
		t.Fatalf("a pass over an unreadable budget = %v, want it not started for that", err)
	}
	if _, err = AdvisorModels(over(company(""), true)).Deps(t.Context(), nil); err != nil {
		t.Fatalf("a pass for a company that caps nothing asked the store: %v", err)
	}
}
