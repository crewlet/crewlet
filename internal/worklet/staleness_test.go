package worklet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY READER CARRIES THE CALLER'S STALENESS BOUND INTO THE FRAMEWORK'S OWN
// QUERY, and this is the hop nothing above this package can see.
//
// Whether a caller's bound reaches [statelog.Query.MaxLagSeq] is a line inside
// each reader, and a reader that dropped it answers exactly as one that
// honoured it (same rows, `read_level` and position). Only a node BEHIND the
// caller's bound, required to refuse, tells them apart.
func TestEveryReaderRefusesPastTheCallersOwnStalenessBound(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	// FAR BEHIND, so a bound of one record and of one second are both
	// exceeded: a reader that carried only one unit must still go red.
	log, err := statelogtest.LocalReaderBehind(worklet.Domain{}, h.db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1, Seq: h.seq},
		100_000)
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(h.db.Reader(), log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	ctx, now := t.Context(), time.Now().UTC()
	const stale = statelog.ReadStale

	for _, tc := range []struct {
		what string
		// seconds and records run the same question once per unit: a
		// reader may thread one field and forget the other.
		seconds func() error
		records func() error
	}{
		{"work_items",
			func() error {
				_, err := reader.Tasks(ctx, worklet.Query{
					Level: stale, MaxLag: time.Second,
					Scope: worklet.Scope{Workspace: true},
				}, now)
				return err
			},
			func() error {
				_, err := reader.Tasks(ctx, worklet.Query{
					Level: stale, MaxLagSeq: 1,
					Scope: worklet.Scope{Workspace: true},
				}, now)
				return err
			}},
		{"work_views",
			func() error {
				_, err := reader.Views(ctx, worklet.ViewQuery{
					Level: stale, MaxLag: time.Second,
					Container: worklet.Container{Kind: worklet.ContainerWorkspace},
				})
				return err
			},
			func() error {
				_, err := reader.Views(ctx, worklet.ViewQuery{
					Level: stale, MaxLagSeq: 1,
					Container: worklet.Container{Kind: worklet.ContainerWorkspace},
				})
				return err
			}},
		{"work_catalogue",
			func() error {
				_, err := reader.Catalogue(ctx, worklet.CatalogueQuery{Level: stale, MaxLag: time.Second})
				return err
			},
			func() error {
				_, err := reader.Catalogue(ctx, worklet.CatalogueQuery{Level: stale, MaxLagSeq: 1})
				return err
			}},
		{"work_person",
			func() error {
				_, err := reader.Person(ctx, worklet.PersonQuery{
					Handle: "ana", Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.Person(ctx, worklet.PersonQuery{
					Handle: "ana", Level: stale, MaxLagSeq: 1,
				}, now)
				return err
			}},
		{"work_projects",
			func() error {
				_, err := reader.Projects(ctx, worklet.ProjectQuery{
					Archived: worklet.ArchivedExclude,
					Level:    stale, MaxLag: time.Second,
				})
				return err
			},
			func() error {
				_, err := reader.Projects(ctx, worklet.ProjectQuery{
					Archived: worklet.ArchivedExclude,
					Level:    stale, MaxLagSeq: 1,
				})
				return err
			}},
		{"work_project",
			func() error {
				_, err := reader.Project(ctx, worklet.ProjectDetailQuery{
					Project: "ENG", Level: stale, MaxLag: time.Second,
				})
				return err
			},
			func() error {
				_, err := reader.Project(ctx, worklet.ProjectDetailQuery{
					Project: "ENG", Level: stale, MaxLagSeq: 1,
				})
				return err
			}},
		{"work_activity",
			func() error {
				_, err := reader.Activity(ctx, worklet.ActivityQuery{
					Workspace: true, Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.Activity(ctx, worklet.ActivityQuery{
					Workspace: true, Level: stale, MaxLagSeq: 1,
				}, now)
				return err
			}},
		{"work_my_work",
			func() error {
				_, err := reader.MyWork(ctx, worklet.MyWorkQuery{
					Handle: "ana", Level: stale, MaxLag: time.Second,
				}, now, time.UTC)
				return err
			},
			func() error {
				_, err := reader.MyWork(ctx, worklet.MyWorkQuery{
					Handle: "ana", Level: stale, MaxLagSeq: 1,
				}, now, time.UTC)
				return err
			}},
		// THE POINT READS TOO: the bound is about this node's LAG, and one
		// row's read is as far behind as a listing's.
		{"work_item",
			func() error {
				_, err := reader.Task(ctx, "ENG-1", worklet.DetailWants{},
					statelog.Freshness{Level: stale, MaxLag: time.Second})
				return err
			},
			func() error {
				_, err := reader.Task(ctx, "ENG-1", worklet.DetailWants{},
					statelog.Freshness{Level: stale, MaxLagSeq: 1})
				return err
			}},
		{"thread",
			func() error {
				_, err := reader.Thread(ctx, worklet.ThreadQuery{Task: "t-1"},
					statelog.Freshness{Level: stale, MaxLag: time.Second})
				return err
			},
			func() error {
				_, err := reader.Thread(ctx, worklet.ThreadQuery{Task: "t-1"},
					statelog.Freshness{Level: stale, MaxLagSeq: 1})
				return err
			}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			for unit, run := range map[string]func() error{
				"max_lag_seconds": tc.seconds, "max_lag_seq": tc.records,
			} {
				if err := run(); !errors.Is(err, statelog.ErrUnavailable) {
					t.Errorf("%s with %s set answered %v on a node 100 000 "+
						"records behind — a bound the reader never carries is "+
						"a bound that never bounded anything", tc.what, unit, err)
				}
			}
		})
	}
}

// THE CONTROL: with no bound, the same reads on the same behind node ANSWER.
//
// Without it every case above would pass against a reader that refused
// everything, which a test of the refusal alone cannot see.
func TestAReaderWithNoBoundAnswersHoweverFarBehindItIs(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	log, err := statelogtest.LocalReaderBehind(worklet.Domain{}, h.db,
		statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1, Seq: h.seq},
		100_000)
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := worklet.NewReader(h.db.Reader(), log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	ctx := t.Context()
	if _, err := reader.Projects(ctx, worklet.ProjectQuery{
		Archived: worklet.ArchivedExclude,
		Level:    statelog.ReadStale,
	}); err != nil {
		t.Errorf("an unbounded stale read refused: %v", err)
	}
}
