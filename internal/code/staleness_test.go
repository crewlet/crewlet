package code_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// EVERY CODE READER CARRIES THE CALLER'S STALENESS BOUND INTO THE FRAMEWORK'S
// OWN QUERY — the hop nothing above this package can see.
//
// A reader that drops the bound produces an answer identical in every visible
// respect to one that honoured it, so the only way to observe the difference
// is to put the node BEHIND the bound and require the refusal — in both units,
// because a reader that threads one field and forgets the other is the exact
// shape the tracker shipped. A push's outcome and the retention pass's two
// reads take no caller bound — the outcome and the pass's confirmation of a
// purge are always read at their record's own position, and its listing is
// the pass's own stale read — so they are not rows here.
func TestEveryCodeReaderRefusesPastTheCallersOwnStalenessBound(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	behind, err := statelogtest.LocalReaderBehind(code.Domain{}, n.db, n.waiter.Committed(), 100_000)
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := code.NewReader(code.ReaderOptions{Log: behind})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	ctx := t.Context()
	seconds := statelog.Freshness{Level: statelog.ReadStale, MaxLag: time.Second}
	records := statelog.Freshness{Level: statelog.ReadStale, MaxLagSeq: 1}
	for _, tc := range []struct {
		what string
		run  func(statelog.Freshness) error
	}{
		{"Resolve", func(f statelog.Freshness) error {
			_, err := reader.Resolve(ctx, code.ResolveQuery{Name: "app", Fresh: f})
			return err
		}},
		{"Snapshot", func(f statelog.Freshness) error {
			_, err := reader.Snapshot(ctx, code.StateQuery{RepoID: id, Fresh: f})
			return err
		}},
		{"Read(repos)", func(f statelog.Freshness) error {
			_, err := reader.Read(ctx, code.ReadQuery{Asker: asker, Kind: code.ReadRepos, Fresh: f})
			return err
		}},
		{"Read(repo)", func(f statelog.Freshness) error {
			_, err := reader.Read(ctx, code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: "app", Fresh: f})
			return err
		}},
		{"Read(pushes)", func(f statelog.Freshness) error {
			_, err := reader.Read(ctx, code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app", Fresh: f})
			return err
		}},
		{"Read(refs)", func(f statelog.Freshness) error {
			_, err := reader.Read(ctx, code.ReadQuery{Asker: asker, Kind: code.ReadRefs, Repo: "app", Fresh: f})
			return err
		}},
		{"Read(introduced)", func(f statelog.Freshness) error {
			_, err := reader.Read(ctx, code.ReadQuery{Kind: code.ReadIntroduced,
				Introduced: &code.IntroducedQuery{RepoID: id}, Fresh: f})
			return err
		}},
		{"Read(run_pushes)", func(f statelog.Freshness) error {
			_, err := reader.Read(ctx, code.ReadQuery{Kind: code.ReadRunPushes,
				RunPushes: &code.RunPushesQuery{Run: runA, Repos: []string{id}, After: n.waiter.Committed()},
				Fresh:     f})
			return err
		}},
		{"ResolveRef", func(f statelog.Freshness) error {
			// ANSWERED even where the repository holds no main: the answer
			// says so of the name, which only a read that was served can.
			_, err := reader.ResolveRef(ctx, code.RefQuery{Asker: asker, Repo: "app", Refs: []string{"main"},
				Fresh: f})
			return err
		}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			for unit, ask := range map[string]statelog.Freshness{
				"max_lag_seconds": seconds, "max_lag_seq": records,
			} {
				if err := tc.run(ask); !errors.Is(err, statelog.ErrUnavailable) {
					t.Errorf("%s with %s set answered %v on a node 100 000 records "+
						"behind — a bound the reader never carries never bounded anything",
						tc.what, unit, err)
				}
			}
			// THE CONTROL: unbounded, the same read on the same node answers.
			if err := tc.run(statelog.Freshness{Level: statelog.ReadStale}); err != nil {
				t.Errorf("%s with no bound refused: %v", tc.what, err)
			}
		})
	}
}

// AND THE FLOOR REACHES THE READ: a caller that hands back the position its
// own write answered with is served the rows at or past it — never the ones
// from before, and on a node that has not applied it, a `behind` refusal.
func TestACodeReadWaitsForTheFloorTheCallerNamed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	n.hold()
	res, err := n.pushErr(id, entry("p1", set("refs/heads/main", code.ZeroOID, oidOf("c1"))))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Position.IsZero() {
		t.Fatalf("the push answered no position: %+v", res)
	}
	ask := statelog.FloorAt(statelog.ReadStale, res.Position)
	for name, read := range map[string]func() error{
		"Snapshot": func() error {
			_, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: ask})
			return err
		},
		"Read(repo)": func() error {
			_, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: "app", Fresh: ask})
			return err
		},
		"Resolve": func() error {
			_, err := n.reader.Resolve(t.Context(), code.ResolveQuery{Name: "app", Fresh: ask})
			return err
		},
		"ResolveRef": func() error {
			_, err := n.reader.ResolveRef(t.Context(), code.RefQuery{Asker: asker, Repo: "app",
				Refs: []string{"main"}, Fresh: ask})
			return err
		},
		"Read(introduced) at session": func() error {
			_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadIntroduced,
				Introduced: &code.IntroducedQuery{RepoID: id},
				Fresh:      statelog.FloorAt(statelog.ReadSession, res.Position)})
			return err
		},
		// A RUN'S LATER PAGE, at session at the position its first page was
		// read at: a node that has not applied it is behind the run's
		// pushes, never a node that answers fewer of them.
		"Read(run_pushes) at session": func() error {
			_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadRunPushes,
				RunPushes: &code.RunPushesQuery{Run: runA, Repos: []string{id}, After: statelog.Position{
					Stream: res.Position.Stream, Generation: res.Position.Generation, Seq: 1}},
				Fresh: statelog.FloorAt(statelog.ReadSession, res.Position)})
			return err
		},
	} {
		var refused *statelog.Refused
		if err := read(); !errors.As(err, &refused) || refused.Code != statelog.RefuseBehind {
			t.Errorf("%s floored at an unapplied push = %v, want refused behind", name, err)
		}
	}
	n.release()
	snap, err := n.reader.Snapshot(t.Context(), code.StateQuery{RepoID: id, Fresh: ask})
	if err != nil || len(snap.Refs) != 1 || snap.Refs[0].OID != oidOf("c1") {
		t.Fatalf("the floored snapshot once applied = %+v, %v; want the pushed ref", snap.Refs, err)
	}
}
