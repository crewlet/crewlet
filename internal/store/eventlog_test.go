package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// EVERY READ IS FLOORED AT THE INSTANT IT IS ASKED AT, never at a fresh read of
// the log's own clock.
//
// A fleet asks every node one question at the ASKER'S instant, and an answer
// assembled from several reads — a turn's rows beside its count, its ending
// and its traces — is one answer only if every read is floored at the same
// one. So every read takes the instant: a query type as `At`, every other read
// as its last argument. The row here sits a minute above the floor under a
// pinned instant an hour back — below the floor under now, and still on disk,
// since retention keeps a day past it — so each read finds it when asked at the
// pinned instant and does not when asked at now (the zero instant).
//
// Mutation: floor any one of these reads at `now()` rather than the instant it
// is handed and its pinned case loses the row; drop ByID's floor and its
// unpinned case finds a row past the horizon.
func TestEveryEventReadIsFlooredAtTheInstantItIsAskedAt(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	edge := at.Add(-store.EventHistory).Add(time.Minute)
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "edge", Type: "agent_phase_completed", Category: "agent", Time: edge,
		TraceID: "tr-edge", Actor: "PM",
		Tags:    map[string]string{"turn_id": "tn-edge", "agent_id": "id-pm", "agent_role": "PM"},
		Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	ctx := t.Context()
	for _, c := range []struct {
		name string
		read func(at time.Time) (int, error)
	}{
		{"List", func(at time.Time) (int, error) {
			rows, err := log.List(ctx, store.ListQuery{At: at})
			return len(rows), err
		}},
		{"List by related agent", func(at time.Time) (int, error) {
			rows, err := log.List(ctx, store.ListQuery{RelatedAgent: "PM", At: at})
			return len(rows), err
		}},
		// THE CHIPS, which take the caller's own edges and so reach down to
		// the floor itself. The bars begin at the first whole day above it,
		// and a row a minute above the floor is in the day the floor cuts —
		// see TestAWindowTheFloorClipsBeginsAtItsFirstWholeBar, and
		// TestEveryCountOnTheAxisIsFlooredAtTheInstantItIsCutAgainst for
		// the bars' own floor.
		{"Histogram", func(at time.Time) (int, error) {
			h, err := log.Histogram(ctx, store.HistogramQuery{
				ListQuery: store.ListQuery{At: at}, Bucket: store.BucketDay,
			})
			return h.ByCategory["agent"], err
		}},
		{"TraceRows", func(at time.Time) (int, error) {
			rows, err := log.TraceRows(ctx, []string{"tr-edge"}, 10, at)
			return len(rows), err
		}},
		{"Trace", func(at time.Time) (int, error) {
			rows, err := log.Trace(ctx, "tr-edge", at)
			return len(rows), err
		}},
		{"TraceEventCount", func(at time.Time) (int, error) {
			return log.TraceEventCount(ctx, "tr-edge", at)
		}},
		{"Turn", func(at time.Time) (int, error) {
			rows, err := log.Turn(ctx, "tn-edge", at)
			return len(rows), err
		}},
		{"TurnEventCount", func(at time.Time) (int, error) {
			return log.TurnEventCount(ctx, "tn-edge", at)
		}},
		{"TurnClosing", func(at time.Time) (int, error) {
			rows, err := log.TurnClosing(ctx, "tn-edge", 5, at)
			return len(rows), err
		}},
		{"TurnTraces", func(at time.Time) (int, error) {
			traces, err := log.TurnTraces(ctx, "tn-edge", at)
			return len(traces), err
		}},
		{"ByID", func(at time.Time) (int, error) {
			_, err := log.ByID(ctx, "edge", at)
			if errors.Is(err, store.ErrNotFound) {
				return 0, nil
			}
			return 1, err
		}},
		{"Phases", func(at time.Time) (int, error) {
			rows, _, err := log.Phases(ctx, "", 10, nil, at)
			return len(rows), err
		}},
		{"AgentPhases", func(at time.Time) (int, error) {
			rows, _, err := log.AgentPhases(ctx, "id-pm", "", nil, at)
			return len(rows), err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pinned, err := c.read(at)
			if err != nil {
				t.Fatalf("asked at the pinned instant: %v", err)
			}
			if pinned != 1 {
				t.Errorf("asked at %s it answers %d, want the row a minute above the floor under it", at, pinned)
			}
			unpinned, err := c.read(time.Time{})
			if err != nil {
				t.Fatalf("asked at now: %v", err)
			}
			if unpinned != 0 {
				t.Errorf("asked at now it answers %d, want nothing — the row is past the horizon", unpinned)
			}
		})
	}
}

// A PAGE AND ITS TRACE SIBLINGS ARE ONE SET, floored once.
//
// A related-agent page is two reads — the rows naming the seat, then the rows
// sharing their traces, which is how the webhook that caused the work lands
// beside it — and the siblings read the clock a query after the page did, so a
// trace straddling the floor kept its direct match while the cause beside it
// went missing. Both are floored at the page's [store.ListQuery.At] now, which
// is also what a fleet pins on every node; the cause here sits above the floor
// under that instant and below the clock's.
//
// Mutation: floor the siblings at `now()` and the cause drops off the page;
// floor the page at `now()` and the whole page is empty.
func TestAListingAndItsTraceSiblingsShareOneFloor(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour)
	floor := at.Add(-store.EventHistory)
	for _, r := range []store.EventRecord{
		// The cause: names nobody, shares the trace.
		{ID: "cause", Type: "webhook_received", Category: "webhook", Time: floor.Add(time.Minute)},
		// The work it caused: names the seat.
		{ID: "work", Type: "agent_phase_completed", Category: "agent", Time: floor.Add(2 * time.Minute),
			Actor: "PM"},
		// Below the floor under the pinned instant: on no page.
		{ID: "older", Type: "webhook_received", Category: "webhook", Time: floor.Add(-time.Minute)},
	} {
		r.TraceID, r.Payload = "tr-1", []byte(`{}`)
		if err := log.Append(t.Context(), r); err != nil {
			t.Fatalf("append %s: %v", r.ID, err)
		}
	}

	rows, err := log.List(t.Context(), store.ListQuery{RelatedAgent: "PM", At: at, Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, []string{"work", "cause"}) {
		t.Errorf("the page is %v, want the work and the cause beside it — both above the "+
			"floor under the instant it was asked at, and nothing below it", ids)
	}
}
