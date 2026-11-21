package main

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/logging"
)

// teardown records the order a node's parts are taken down in.
type teardown struct{ steps []string }

type teardownEngine struct{ t *teardown }

func (e teardownEngine) Drain(context.Context) { e.t.steps = append(e.t.steps, "drain") }
func (e teardownEngine) Stop(context.Context)  { e.t.steps = append(e.t.steps, "engine") }

type teardownListeners struct{ t *teardown }

func (l teardownListeners) finishGit(context.Context) { l.t.steps = append(l.t.steps, "git") }

func (l teardownListeners) stop(context.Context, *slog.Logger) {
	l.t.steps = append(l.t.steps, "listeners")
}

type teardownHub struct{ t *teardown }

func (h teardownHub) Stop(context.Context) { h.t.steps = append(h.t.steps, "hub") }

// A NODE COMES DOWN IN ONE ORDER, each step's reason a step before it:
//   - DRAIN first, listener up: probes answer while turns finish; it refuses new work.
//   - GIT REQUESTS next, listener up: a cut clone or push restarts from nothing
//     on a peer, and a running request reads the estate the engine closes.
//   - LISTENERS after both: closed first, a node failed liveness doing its job.
//   - CHAT HUB after the drain: finishing turns post replies a peer's room learns of.
//   - ENGINE last: it closes the broker and store the hub and probes use.
//
// A node with no listener or hub comes down the same way (runEngine passes nil
// parts there). Mutation: swap any two steps of [shutdown], or drop the nil
// check from [httpSurface.stop] or [httpSurface.finishGit], and this fails.
func TestShutdownTakesANodeDownInOneOrder(t *testing.T) {
	t.Parallel()
	got := &teardown{}
	shutdown(t.Context(), teardownEngine{got}, teardownListeners{got}, teardownHub{got},
		logging.Get("test"))
	if want := []string{"drain", "git", "listeners", "hub", "engine"}; !slices.Equal(got.steps, want) {
		t.Errorf("a node came down as %v, want %v", got.steps, want)
	}

	none := &teardown{}
	var surface *httpSurface
	var hub *api.EngineChatLive
	shutdown(t.Context(), teardownEngine{none}, surface, hub, logging.Get("test"))
	if want := []string{"drain", "engine"}; !slices.Equal(none.steps, want) {
		t.Errorf("a node with no listener and no hub came down as %v, want %v",
			none.steps, want)
	}
}
