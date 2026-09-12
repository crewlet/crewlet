package tracker_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY READER CARRIES THE CALLER'S STALENESS BOUND INTO THE FRAMEWORK'S OWN
// QUERY, and this is the hop nothing above this package can see.
//
// A surface builds a `tracker.GoalQuery` with `MaxLagSeq: 250` and hands it
// over; whether the field then reaches [statelog.Query.MaxLagPositions] is a
// line inside each reader, and a reader that dropped it produces an answer
// identical in every visible respect to one that honoured it — same rows, same
// `read_level`, same position. The only way to observe the difference is to
// put the node BEHIND the caller's bound and require the refusal.
//
// That is the defect this file exists for rather than a hypothetical: the
// board's `max_lag_seconds` was checked against the level and then dropped, so
// a tile polling every twenty seconds and declaring a twenty-second bound was
// served an answer of any age and rendered it live. `max_lag_seq` was the same
// shape, fixed for the board and left standing for nine other questions that
// never read the key at all.
func TestEveryReaderRefusesPastTheCallersOwnStalenessBound(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	// FAR BEHIND, so a bound of one record and of one second are both
	// exceeded — the two units are two readings of one distance and a
	// reader that carried only one of them must still go red.
	log, err := statelogtest.LocalReaderBehind(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name, Generation: 1, Seq: h.seq},
		100_000)
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	ctx, now := t.Context(), time.Now().UTC()
	const stale = statelog.ReadStale

	for _, tc := range []struct {
		what string
		// seconds and records run the same question twice, once per
		// unit, because a reader that threads one field and forgets the
		// other is the exact shape this package already shipped.
		seconds func() error
		records func() error
	}{
		{"work_items",
			func() error {
				_, err := reader.Tasks(ctx, tracker.Query{
					Level: stale, MaxLag: time.Second,
					Scope: tracker.Scope{Workspace: true},
				}, now)
				return err
			},
			func() error {
				_, err := reader.Tasks(ctx, tracker.Query{
					Level: stale, MaxLagSeq: 1,
					Scope: tracker.Scope{Workspace: true},
				}, now)
				return err
			}},
		{"work_views",
			func() error {
				_, err := reader.Views(ctx, tracker.ViewQuery{
					Level: stale, MaxLag: time.Second,
					Container: tracker.Container{Kind: tracker.ContainerWorkspace},
				})
				return err
			},
			func() error {
				_, err := reader.Views(ctx, tracker.ViewQuery{
					Level: stale, MaxLagSeq: 1,
					Container: tracker.Container{Kind: tracker.ContainerWorkspace},
				})
				return err
			}},
		{"work_goals",
			func() error {
				_, err := reader.Goals(ctx, tracker.GoalQuery{Level: stale, MaxLag: time.Second})
				return err
			},
			func() error {
				_, err := reader.Goals(ctx, tracker.GoalQuery{Level: stale, MaxLagSeq: 1})
				return err
			}},
		{"work_catalogue",
			func() error {
				_, err := reader.Catalogue(ctx, tracker.CatalogueQuery{Level: stale, MaxLag: time.Second})
				return err
			},
			func() error {
				_, err := reader.Catalogue(ctx, tracker.CatalogueQuery{Level: stale, MaxLagSeq: 1})
				return err
			}},
		{"work_person",
			func() error {
				_, err := reader.Person(ctx, tracker.PersonQuery{
					Handle: "ana", Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.Person(ctx, tracker.PersonQuery{
					Handle: "ana", Level: stale, MaxLagSeq: 1,
				}, now)
				return err
			}},
		{"work_projects",
			func() error {
				_, err := reader.Projects(ctx, tracker.ProjectQuery{Level: stale, MaxLag: time.Second}, now)
				return err
			},
			func() error {
				_, err := reader.Projects(ctx, tracker.ProjectQuery{Level: stale, MaxLagSeq: 1}, now)
				return err
			}},
		{"work_project",
			func() error {
				_, err := reader.Project(ctx, tracker.ProjectDetailQuery{
					Project: "ENG", Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.Project(ctx, tracker.ProjectDetailQuery{
					Project: "ENG", Level: stale, MaxLagSeq: 1,
				}, now)
				return err
			}},
		{"work_sprints",
			func() error {
				_, err := reader.Sprints(ctx, tracker.SprintQuery{
					Project: "ENG", Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.Sprints(ctx, tracker.SprintQuery{
					Project: "ENG", Level: stale, MaxLagSeq: 1,
				}, now)
				return err
			}},
		{"work_activity",
			func() error {
				_, err := reader.Activity(ctx, tracker.ActivityQuery{
					Workspace: true, Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.Activity(ctx, tracker.ActivityQuery{
					Workspace: true, Level: stale, MaxLagSeq: 1,
				}, now)
				return err
			}},
		{"work_my_work",
			func() error {
				_, err := reader.MyWork(ctx, tracker.MyWorkQuery{
					Handle: "ana", Level: stale, MaxLag: time.Second,
				}, now)
				return err
			},
			func() error {
				_, err := reader.MyWork(ctx, tracker.MyWorkQuery{
					Handle: "ana", Level: stale, MaxLagSeq: 1,
				}, now)
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
// everything — which is the other way to make a staleness bound meaningless,
// and the one a test written only for the refusal cannot see.
func TestAReaderWithNoBoundAnswersHoweverFarBehindItIs(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	log, err := statelogtest.LocalReaderBehind(tracker.Domain{}, h.db.Replicated(),
		statelog.Position{Stream: tracker.Domain{}.Stream().Name, Generation: 1, Seq: h.seq},
		100_000)
	if err != nil {
		t.Fatalf("local read authority: %v", err)
	}
	reader, err := tracker.NewReader(h.db, log)
	if err != nil {
		t.Fatalf("tracker reader: %v", err)
	}
	ctx, now := t.Context(), time.Now().UTC()
	if _, err := reader.Goals(ctx, tracker.GoalQuery{Level: statelog.ReadStale}); err != nil {
		t.Errorf("an unbounded stale read refused: %v — zero accepts anything, "+
			"which is what makes the bound the caller's decision", err)
	}
	if _, err := reader.Projects(ctx, tracker.ProjectQuery{
		Level: statelog.ReadStale,
	}, now); err != nil {
		t.Errorf("an unbounded stale read refused: %v", err)
	}
}
