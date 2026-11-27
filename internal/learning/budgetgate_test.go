package learning_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/learning"
)

// handleGate is a [learning.BudgetGate] answering ok and err, keeping whom it
// was asked for.
type handleGate struct {
	mu    sync.Mutex
	asked []string
	ok    bool
	err   error
}

func (g *handleGate) gate(_ context.Context, handle string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, handle)
	return g.ok, g.err
}

func (g *handleGate) handles() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.asked)
}

var budgetAnswers = []struct {
	name  string
	ok    bool
	err   error
	calls int
}{
	{"no room", false, nil, 0},
	{"an unreadable counter", false, errors.New("store unreachable"), 0},
	{"room", true, nil, 1},
}

// A CLUSTERED PASS THE BUDGET DECLINES DRAFTS NOTHING (#1838), asking for the
// seat it would draft for; one with room drafts.
func TestAClusteringPassTheBudgetDeclinesDraftsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range budgetAnswers {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newStore(t)
			writeEpisodes(t, db, 4, "done", "fetch", "build", "tag", "announce")
			g := &handleGate{ok: tc.ok, err: tc.err}
			s, p := clusterer(t, db, goodDraft, learning.SynthesizerOptions{
				MinToolCalls: 3, ClusterMinSize: 3, Budget: g.gate,
			})
			out, err := s.ClusterPass(t.Context(), clusterSeat, clusterAgentID)
			if err != nil {
				t.Fatalf("ClusterPass: %v", err)
			}
			if p.calls != tc.calls || len(out) != tc.calls {
				t.Fatalf("%d model calls and %d payloads, want %d", p.calls, len(out), tc.calls)
			}
			if got := g.handles(); !slices.Equal(got, []string{"dev"}) {
				t.Errorf("the gate was asked for %v, want the seat once", got)
			}
		})
	}
}

// A PROMOTION THE BUDGET DECLINES DRAFTS NOTHING (#1838), asking for the
// unit's lead, whose chain the pass runs on.
func TestAPromotionTheBudgetDeclinesDraftsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range budgetAnswers {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newStore(t)
			for _, h := range []string{"dev", "sre", "qa"} {
				seedSibling(t, db, h, "release-"+h, "fetch", "build", "tag", "announce")
			}
			g := &handleGate{ok: tc.ok, err: tc.err}
			w := &fakeWriter{}
			p, model := promoter(t, db, w, promotionDraft, unitOf("dev", "sre", "qa"),
				learning.PromoterOptions{MinSiblings: 3, Budget: g.gate})
			out := p.Pass(t.Context())
			if model.calls != tc.calls || len(out) != tc.calls || len(w.calls()) != tc.calls {
				t.Fatalf("%d model calls, %d payloads, %d drafts, want %d of each",
					model.calls, len(out), len(w.calls()), tc.calls)
			}
			if got := g.handles(); !slices.Equal(got, []string{"lead"}) {
				t.Errorf("the gate was asked for %v, want the unit's lead once", got)
			}
		})
	}
}
