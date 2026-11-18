package engine_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/usage"
)

// A DATA NODE WITH NO NATIVE RUNTIME ANSWERS ITS SPEND SCREENS from the estate
// it holds open from boot, as every data node does whatever its company runs.
//
// A company on vendors for both tracker and knowledge base runs no state log,
// so its data nodes have no native runtime, like this node booted with no
// company. Usage reads tied to the runtime were refused as unserved, a 503 the
// screen retried for ever. Through the reads the API is handed
// (Engine.UsageReads), every usage read and the questions built on them answer.
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
	// ASKED AS A CREDENTIAL THAT MAY ASK BOTH, so a refusal can only be the
	// estate answering unserved, never the caller's authority.
	asker := iam.WithPrincipal(ctx, iam.Principal{
		ID: uuid.New(), Login: iam.TokenLogin("test"), Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: iam.AllGrants,
	})
	for _, q := range []struct {
		what   string
		params map[string]any
	}{
		{"token_series", map[string]any{"days": 7}},
		{"page_reads", map[string]any{"page": "p-1"}},
	} {
		if _, err := r.Answer(asker, q.what, q.params); err != nil {
			t.Errorf("%s on a data node with no runtime: %v — want an answer, not a refusal",
				q.what, err)
		}
	}
}
