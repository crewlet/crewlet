package engine

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/github"
)

// A PULL REQUEST PREVIEW IS READ THROUGH THE WIRING IN FORCE AND NO OTHER: a
// company with no GitHub reads nothing, a wiring's answer is reused while it
// is fresh, and retiring or rewiring GitHub drops every answer the old
// credential gave.
//
// Mutation: keep the cache across a rewire and the last read below answers
// the retired wiring's cached failure instead of saying GitHub is gone.
func TestAPullPreviewIsReadThroughTheWiringInForce(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 5, 6, 7, 8, 0, 0, time.UTC)
	g := &githubPullReader{now: func() time.Time { return now }}
	ref := github.PullRef{Owner: "acme", Repo: "web", Number: 41}

	if _, err := g.read(t.Context(), ref); !errors.Is(err, errNoGitHub) {
		t.Fatalf("an unwired reader answered %v, want no GitHub", err)
	}
	g.wire(&github.SeatLookup{})
	if _, err := g.read(t.Context(), ref); !errors.Is(err, github.ErrNoSeatCanRead) {
		t.Fatalf("a wiring with no agent's app answered %v, want no seat can read", err)
	}
	if hit, ok := g.entries[ref]; !ok || !errors.Is(hit.err, github.ErrNoSeatCanRead) {
		t.Errorf("the read was not kept for reuse: %+v", g.entries)
	}
	g.wire(nil)
	if _, err := g.read(t.Context(), ref); !errors.Is(err, errNoGitHub) {
		t.Errorf("a retired wiring answered %v, want no GitHub", err)
	}
}
