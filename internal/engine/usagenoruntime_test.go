package engine_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/usage"
)

// A DATA NODE WITH NO NATIVE RUNTIME ANSWERS ITS SPEND SCREENS from the estate
// it holds open from boot, as every data node does whatever its company runs.
//
// A company on vendors for both its tracker and its knowledge base runs no
// state log, so no data node of it has a native runtime — and neither has a
// data node that booted with no company, which is the node here. With the
// usage reads tied to the runtime, every data node answered every one of them
// "no native backend here", so the router refused each as an estate nobody
// served, and the query surface turned that into 503 `unavailable` with a
// Retry-After: a spend window, a seat's activity and a page's readers each
// asked the screen to retry, for ever, a read the open file could answer.
// Through the very reads the API is handed (Engine.UsageReads), every read of
// the usage domain answers, and the questions built on them are not refused.
func TestADataNodeWithNoRuntimeAnswersItsUsageReads(t *testing.T) {
	t.Parallel()
	e := unconfiguredEngine(t)
	ctx := t.Context()
	u := e.UsageReads()
	day := time.Now().UTC().Format(time.DateOnly)
	days := usage.SpendQuery{From: day, To: day}
	for name, read := range map[string]func() error{
		"spend":      func() error { _, err := u.Spend(ctx, days); return err },
		"seat turns": func() error { _, err := u.SeatTurns(ctx, days); return err },
		"person spend": func() error {
			_, err := u.PersonSpend(ctx, usage.PersonQuery{From: day, To: day})
			return err
		},
		"seat days": func() error { _, err := u.SeatDays(ctx, days); return err },
		"page reads": func() error {
			_, err := u.PageReads(ctx, usage.PageReadsQuery{PageIDs: []string{"p-1"}, From: day, To: day})
			return err
		},
		"schedule runs": func() error {
			_, _, err := u.ScheduleRuns(ctx, usage.ScheduleRunsQuery{Limit: 10})
			return err
		},
		"reads elided": func() error { _, err := u.ReadsElided(ctx, day, day); return err },
	} {
		if err := read(); err != nil {
			t.Errorf("%s on a data node with no runtime: %v — want it answered from the open estate",
				name, err)
		}
	}

	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Usage: u})
	for _, q := range []struct {
		what   string
		params map[string]any
	}{
		{"token_series", map[string]any{"days": 7}},
		{"page_reads", map[string]any{"page": "p-1"}},
	} {
		if _, err := r.Answer(ctx, q.what, q.params, ""); err != nil {
			t.Errorf("%s on a data node with no runtime: %v — want an answer, not a refusal",
				q.what, err)
		}
	}
}
