package api_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
)

// TestReadyIs503WhileTheEstateCannotAnswerAtTheFloor.
//
// A node's SQL copy is derived from a trimmed log. A consumer whose position
// falls below the stream's first sequence is CLAMPED UPWARD with no error at
// all and then reports itself caught up over a hole — so such a node answers
// every question confidently, out of rows that are missing records for good.
// The engine already refuses to admit a seat to a node in that state; this is
// the same question asked for TRAFFIC, so the node that will not take a seat
// also stops being sent work to do with one. A webhook delivered there, or a
// board rendered from it, is an answer somebody acts on: "there is no such
// work item" is how a duplicate gets filed.
func TestReadyIs503WhileTheEstateCannotAnswerAtTheFloor(t *testing.T) {
	t.Parallel()
	var established atomic.Bool
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: active()},
		Estate: func(context.Context) (bool, string) {
			if established.Load() {
				return true, ""
			}
			return false, "below_floor"
		},
	})

	status, body := get(t, a, "/ready")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/ready = %d below the log's floor, want 503: this node's "+
			"rows have a hole nothing will fill", status)
	}
	if body["reason"] != api.ReasonEstateBehind {
		t.Errorf("reason = %v, want %q", body["reason"], api.ReasonEstateBehind)
	}
	if body["ready"] != false {
		t.Errorf("ready = %v, want false", body["ready"])
	}

	// THE CONTROL that makes the refusal above able to fail: with the
	// same app and only the estate's answer changed, the node is ready.
	// Without it, a /ready that answered 503 for some unrelated reason —
	// an unconfigured node, a missing source — would pass the case above.
	established.Store(true)
	status, body = get(t, a, "/ready")
	if status != http.StatusOK || body["ready"] != true {
		t.Errorf("/ready = %d %v once the estate answers at the floor, want "+
			"200 and ready", status, body)
	}
	if _, present := body["reason"]; present {
		t.Errorf("a ready node named a reason: %v", body["reason"])
	}
}

// TestANodeWithNoReplicatedEstateIsReady: a company on Jira and Confluence
// runs no state-log domain whose health gates anything, which the engine
// itself reports as trivially established. An absent seam must not be read as
// "assume the worst" — such a node would never be ready at all.
func TestANodeWithNoReplicatedEstateIsReady(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Sources: queries.Sources{Company: active()}})
	if status, body := get(t, a, "/ready"); status != http.StatusOK {
		t.Errorf("/ready = %d %v with no estate seam, want 200", status, body)
	}
}

// TestTheEstateNeverOutranksADrain pins the precedence.
//
// The estate term is the NARROWEST: a draining node is out of rotation for a
// reason an operator can act on directly, and reporting the estate instead
// would name a consequence over a cause — an operator reading "estate_behind"
// on a node they stopped themselves has been told the wrong thing.
func TestTheEstateNeverOutranksADrain(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{
		Sources: queries.Sources{Company: active()},
		Runtime: &fakeRuntime{state: api.RuntimeState{
			Posture: "serve", ShuttingDown: true,
		}},
		Estate: func(context.Context) (bool, string) { return false, "below_floor" },
	})
	status, body := get(t, a, "/ready")
	if status != http.StatusServiceUnavailable || body["reason"] != api.ReasonDraining {
		t.Errorf("/ready = %d %v during a drain, want 503 naming the drain",
			status, body)
	}
}
