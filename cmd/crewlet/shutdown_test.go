package main

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// teardown records the order a node's parts are taken down in.
type teardown struct{ steps []string }

type teardownEngine struct{ t *teardown }

func (e teardownEngine) Drain(context.Context) { e.t.steps = append(e.t.steps, "drain") }
func (e teardownEngine) Stop(context.Context)  { e.t.steps = append(e.t.steps, "engine") }

type teardownListeners struct{ t *teardown }

func (l teardownListeners) stop(context.Context, *slog.Logger) {
	l.t.steps = append(l.t.steps, "listeners")
}

type teardownHub struct{ t *teardown }

func (h teardownHub) Stop() { h.t.steps = append(h.t.steps, "hub") }

// A NODE COMES DOWN IN ONE ORDER, and each step's reason is a step before it.
//
//   - THE DRAIN FIRST, with the listener up: the probes answer while the turns
//     finish, and the drain is what refuses new work.
//   - THE LISTENERS once it has finished: closed first, a node failed its
//     liveness probe while doing exactly what it should.
//   - THE CHAT HUB after the drain: the turns that finish during it post their
//     replies and lower their indicators, and a peer's room learns of both
//     through this hub — stopped first, it went dark for a rolling restart.
//   - THE ENGINE last: its teardown closes the broker the hub's answerers are
//     subscribed on and the store both probes read.
//
// Nothing else asserts it, and every reversal is silent until production.
func TestShutdownTakesANodeDownInOneOrder(t *testing.T) {
	t.Parallel()
	got := &teardown{}
	shutdown(t.Context(), teardownEngine{got}, teardownListeners{got}, teardownHub{got},
		logging.Get("test"))
	if want := []string{"drain", "listeners", "hub", "engine"}; !slices.Equal(got.steps, want) {
		t.Errorf("a node came down as %v, want %v", got.steps, want)
	}

	// AND A NODE THAT BOUND NO LISTENER comes down the same way, with
	// nothing between the drain and the hub.
	none := &teardown{}
	var surface *httpSurface
	shutdown(t.Context(), teardownEngine{none}, surface, teardownHub{none},
		logging.Get("test"))
	if want := []string{"drain", "hub", "engine"}; !slices.Equal(none.steps, want) {
		t.Errorf("a node with no listener came down as %v, want %v", none.steps, want)
	}
}
