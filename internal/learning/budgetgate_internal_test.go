package learning

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
)

// askedGate is a [BudgetGate] answering ok and err, keeping whom it was asked
// for.
type askedGate struct {
	mu    sync.Mutex
	asked []string
	ok    bool
	err   error
}

func (g *askedGate) gate(_ context.Context, handle string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, handle)
	return g.ok, g.err
}

func (g *askedGate) handles() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

// A COMPACTION THE BUDGET DECLINES FOLDS NOTHING (#1838): no summarizer call,
// its clusters left raw for the next pass, the reason on the result. A counter
// nobody could read declines it as a full window does. The gate is asked
// once, for the seat, before the first fold.
func TestACompactionTheBudgetDeclinesFoldsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		ok    bool
		err   error
		skip  string
		calls int
	}{
		{"no room", false, nil, SkipNoBudget, 0},
		{"an unreadable counter", false, errors.New("store unreachable"), SkipBudgetUnknown, 0},
		{"room", true, nil, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &askedGate{ok: tc.ok, err: tc.err}
			l, e, sum := newLife(t, Options{Budget: g.gate})
			sixSimilar(e, t)
			res := mustPass(t, l)
			if res.BudgetSkip != tc.skip || sum.calls() != tc.calls {
				t.Fatalf("skip %q and %d summarizer calls, want %q and %d", res.BudgetSkip, sum.calls(), tc.skip, tc.calls)
			}
			if got := g.handles(); !slices.Equal(got, []string{"ceo"}) {
				t.Errorf("the gate was asked for %v, want the seat once", got)
			}
			if _, compacted := snapshot(t, e); (len(compacted) == 0) != (tc.calls == 0) {
				t.Errorf("%d compacted rows after %d summarizer calls", len(compacted), tc.calls)
			}
		})
	}
}

// A SEAT WITH NOTHING TO FOLD ASKS NOTHING: no refusal is stamped for a pass
// that would have made no call.
func TestACompactionWithNothingToFoldAsksNoBudget(t *testing.T) {
	t.Parallel()
	g := &askedGate{ok: false}
	l, e, _ := newLife(t, Options{Budget: g.gate})
	write(t, e, rawEp("lone", daysAgo(60), "slack_post", "jira_get"))
	if res := mustPass(t, l); res.BudgetSkip != "" {
		t.Errorf("skip = %q for a pass with nothing to fold", res.BudgetSkip)
	}
	if got := g.handles(); len(got) != 0 {
		t.Errorf("the gate was asked for %v by a pass with nothing to fold", got)
	}
}
