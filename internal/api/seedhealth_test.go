package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// `/health` SAYS THE SEED IS COMPLETE ONCE A SEED THAT MISSED THE BOOT LANDS.
//
// A node whose history read outlasted the boot's budget answered
// `seeded_from.complete: false` for the life of the process, so the one place
// that says whether the feed and each seat's last turn carry history said no
// until the next restart, every restart.
//
// Mutation: cancel the seeds still being read when Seeding.Wait's budget runs
// out, and `seeded_from` never appears.
func TestHealthSaysTheSeedIsCompleteOnceALateSeedLands(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		State:   livestate.New(),
		Sources: queries.Sources{Company: active(t)},
	})
	history := &releasedHistory{release: make(chan struct{})}
	seeding := observe.StartSeeding(t.Context(), observe.HistorySeed(history,
		func() *org.Organization { return nil }, a.Stream()))
	t.Cleanup(seeding.Stop)

	// THE READ IS HELD, so the handover's budget passes with it pending
	// however long the budget is.
	if pending := seeding.Wait(t.Context(), time.Millisecond); len(pending) != 1 {
		t.Fatalf("pending at the handover = %v, want the held history seed", pending)
	}
	if _, body := get(t, a, "/health"); body["seeded_from"] != nil {
		t.Fatalf("seeded_from = %v before any seed landed", body["seeded_from"])
	}

	close(history.release)
	if pending := seeding.Wait(t.Context(), time.Minute); len(pending) > 0 {
		t.Fatalf("still pending after its read was released: %v", pending)
	}
	_, body := get(t, a, "/health")
	seeded, _ := body["seeded_from"].(map[string]any)
	if seeded == nil || seeded["complete"] != true {
		t.Errorf("seeded_from = %v, want the late seed's complete coverage", body["seeded_from"])
	}
}

// releasedHistory is one node whose feed read answers once released.
type releasedHistory struct{ release chan struct{} }

func (h *releasedHistory) List(ctx context.Context, _ store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	select {
	case <-h.release:
	case <-ctx.Done():
		return eventfan.Listing{}, eventfan.Coverage{}, ctx.Err()
	}
	return eventfan.Listing{}, oneNode(), nil
}

func (h *releasedHistory) PhaseTokens(context.Context, store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	return nil, oneNode(), nil
}

func (h *releasedHistory) Turns(context.Context, store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	return eventfan.TurnPage{}, oneNode(), nil
}

// oneNode is the coverage of a read the only node answered.
func oneNode() eventfan.Coverage {
	return eventfan.Coverage{Complete: true, Nodes: []eventfan.NodeCoverage{{ID: "node-1", Answered: true}}}
}
