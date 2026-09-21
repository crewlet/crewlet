package pages_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// EVERY PAGE READER CARRIES THE CALLER'S STALENESS BOUND INTO THE FRAMEWORK'S
// OWN QUERY — the hop nothing above this package can see.
//
// The three page reads took a LEVEL and nothing else, so a dashboard poll
// declaring `max_lag_seq=250` on a listing was served an answer of any
// distance and told, in the answer's own `read_level`, that it came back at
// the level asked for. A reader that drops the bound produces an answer
// identical in every visible respect to one that honoured it; the only way
// to observe the difference is to put the node BEHIND the bound and require
// the refusal — in both units, because a reader that threads one field and
// forgets the other is the exact shape the tracker already shipped.
func TestEveryPageReaderRefusesPastTheCallersOwnStalenessBound(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	behind, err := statelogtest.LocalReaderBehind(pages.Domain{}, r.db.Replicated(),
		statelog.Position{Stream: pages.Domain{}.Stream().Name, Generation: 1, Seq: 1},
		100_000)
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := pages.NewReader(pages.ReaderOptions{Log: behind})
	if err != nil {
		t.Fatalf("pages reader: %v", err)
	}
	ctx := t.Context()
	const stale = statelog.ReadStale
	seconds := statelog.Freshness{Level: stale, MaxLag: time.Second}
	records := statelog.Freshness{Level: stale, MaxLagSeq: 1}

	for _, tc := range []struct {
		what string
		run  func(statelog.Freshness) error
	}{
		{"pages", func(f statelog.Freshness) error {
			_, err := reader.List(ctx, pages.Filter{}, f)
			return err
		}},
		{"page", func(f statelog.Freshness) error {
			_, err := reader.Get(ctx, "ENG/Deploy Runbook", f)
			return err
		}},
		{"containers", func(f statelog.Freshness) error {
			_, err := reader.Containers(ctx, f)
			return err
		}},
		{"skill_pages", func(f statelog.Freshness) error {
			_, err := reader.SkillPages(ctx, "SKILLS", f)
			return err
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			for unit, ask := range map[string]statelog.Freshness{
				"max_lag_seconds": seconds, "max_lag_seq": records,
			} {
				if err := tc.run(ask); !errors.Is(err, statelog.ErrUnavailable) {
					t.Errorf("%s with %s set answered %v on a node 100 000 "+
						"records behind — a bound the reader never carries is "+
						"a bound that never bounded anything", tc.what, unit, err)
				}
			}
			// THE CONTROL: unbounded, the same read on the same node
			// ANSWERS (or misses its page), so the cases above are not
			// passing against a reader that refuses everything.
			if err := tc.run(statelog.Freshness{Level: stale}); errors.Is(err, statelog.ErrUnavailable) {
				t.Errorf("%s with no bound refused: %v — zero accepts "+
					"anything, which is what makes the bound the caller's "+
					"decision", tc.what, err)
			}
		})
	}
}

// AND THE FLOOR REACHES THE READ: a caller that hands back the position its
// own save answered with is served the body at or past it, never the one
// from before — on a node that has applied the save that is the same answer
// after a wait for nothing, and on one that has not it is a `behind` refusal
// rather than the stale body wearing the caller's own position.
func TestAPageReadWaitsForTheFloorTheCallerNamed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Title: "Deploy Runbook", Body: "first body"})
	saved, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("second body")})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Outcome.Position.IsZero() {
		t.Fatalf("the save answered with no position: %+v", saved.Outcome)
	}
	// A READER THAT CAN WAIT, over the harness's own waiter — the one
	// [newRoundTrip] builds is already at its position and never blocks,
	// which would make "not yet applied" indistinguishable from applied.
	waiting, err := statelogtest.LocalReaderOver(pages.Domain{}, r.db.Replicated(), r.waiter)
	if err != nil {
		t.Fatalf("waiting read authority: %v", err)
	}
	reader, err := pages.NewReader(pages.ReaderOptions{
		Log: waiting, Committed: r.waiter.Committed,
	})
	if err != nil {
		t.Fatalf("pages reader: %v", err)
	}
	// NOT DRAINED: this node has not applied the save, so a read that
	// honours the floor cannot be served from what it holds.
	ask := statelog.Freshness{Level: statelog.ReadStale, MinPosition: saved.Outcome.Position}
	_, err = reader.Get(t.Context(), page.Page.ID, ask)
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBehind {
		t.Fatalf("a stale read floored at an unapplied save = %v, want a "+
			"`behind` refusal rather than the body from before it", err)
	}
	r.drain()
	got, err := reader.Get(t.Context(), page.Page.ID, ask)
	if err != nil {
		t.Fatalf("the same read once applied: %v", err)
	}
	if got.Page.Body != "second body" {
		t.Fatalf("the floored read served %q, want the saved body", got.Page.Body)
	}
}
