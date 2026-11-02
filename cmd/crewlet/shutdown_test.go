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

// A NODE COMES DOWN IN ONE ORDER, and each step's reason is a step before it.
//
//   - THE DRAIN FIRST, with the listener up: the probes answer while the turns
//     finish, and the drain is what refuses new work.
//   - THE GIT REQUESTS the node is serving once it has finished, with the
//     listener still up: a clone or a push the close cut is begun again from
//     nothing on a peer, and a request still running reads the estate the
//     engine's teardown closes.
//   - THE LISTENERS once both have finished: closed first, a node failed its
//     liveness probe while doing exactly what it should.
//   - THE CHAT HUB after the drain: the turns that finish during it post their
//     replies and lower their indicators, and a peer's room learns of both
//     through this hub — stopped first, it went dark for a rolling restart.
//   - THE ENGINE last: its teardown closes the broker the hub's answerers are
//     subscribed on and the store both probes read, and its appliers announce
//     into whatever hub is still attached.
//
// Nothing else asserts it, and every reversal is silent until production. And
// a node that bound no listener, or a boot that never built a hub, comes down
// the same way with nothing in between — the nil parts are the ones runEngine
// hands over on exactly those nodes. Mutation: swap any two steps of
// [shutdown], or drop the nil check from [httpSurface.stop] or
// [httpSurface.finishGit], and this fails.
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
