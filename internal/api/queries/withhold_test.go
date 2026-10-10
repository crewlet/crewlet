package queries_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// spendingWork is a tracker that has charged its tasks, and that fills a
// board row's cost exactly when the question asks for it — the real reader's
// contract ([tracker.RowFieldSpend]), so the gate below judges the surface and
// not a stub that decorates every row.
type spendingWork struct{ *stubWork }

func (w spendingWork) Tasks(_ context.Context, q tracker.Query, _ time.Time) (tracker.Answer, error) {
	w.query = q
	row := tracker.TaskRow{ID: "t-1", Key: "ENG-1", Title: "Ship it"}
	if q.Wants(tracker.RowFieldSpend) {
		row.Spend = &tracker.RowSpend{Tokens: 4200, Turns: 3, Workers: 1}
	}
	return tracker.Answer{Rows: []tracker.TaskRow{row}}, nil
}

// newSpendingWork charges a task, its detail and its completion on the feed.
func newSpendingWork() spendingWork {
	first := true
	return spendingWork{&stubWork{
		detail: tracker.TaskDetail{Task: tracker.Task{ID: "t-1", Key: "ENG-1", Title: "Ship it",
			Spend: tracker.Spend{Turns: 3, Input: 4000, Output: 200, Tokens: 4200, Workers: 1}}},
		feed: tracker.FeedPage{Rows: []tracker.FeedRow{{
			ID: "h-1", Kind: tracker.FeedCompleted, Task: "t-1", Key: "ENG-1",
			At:        time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC),
			Spend:     &tracker.FeedSpend{Tokens: 4200, Turns: 3},
			FirstPass: &first,
		}}},
	}}
}

// spendCarried is every place a rendered answer names what the machine spent:
// a key `spend` or `tokens`, or one under a `spend_` column's name.
func spendCarried(t *testing.T, answer any) []string {
	t.Helper()
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found []string
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for key, inner := range v {
				if key == "spend" || key == "tokens" || strings.HasPrefix(key, tracker.SpendPrefix) {
					found = append(found, path+"."+key)
				}
				walk(path+"."+key, inner)
			}
		case []any:
			for _, inner := range v {
				walk(path+"[]", inner)
			}
		}
	}
	walk("answer", decoded)
	slices.Sort(found)
	return found
}

// NO MEMBER ANSWER CARRIES SPEND (ADR-0031). What a task, a completion or a
// seat cost is what the machine processed, and a member key reads what the
// company published: each member answer that can carry a cost is rendered at
// both reaches over a fixture that has spent, and the member's must name none
// of it while the admin's — the control, without which an empty fixture
// passes — names it.
func TestNoMemberAnswerCarriesSpend(t *testing.T) {
	t.Parallel()
	usageFixture := newSpendFixture(t)
	usageFixture.seatDay("node-a", "2026-09-24", "lead", usage.Turns{Count: 3},
		spent("execute", "m", 4200))
	work := func() queries.Sources {
		s := partySources(t, nil)
		s.Work = newSpendingWork()
		return s
	}
	cases := []struct {
		what    string
		params  map[string]any
		sources func() queries.Sources
	}{
		{"work_item", map[string]any{"id": "ENG-1"}, work},
		{"work_items", map[string]any{"container": "workspace", "fields": "spend"}, work},
		{"company_feed", map[string]any{"kinds": "completed"}, work},
		{"seat_activity", map[string]any{"days": 7, "previous": true}, usageFixture.sources},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			admin, err := registryOver(t, tc.sources()).Answer(t.Context(), tc.what, tc.params,
				asAdmin("ops-1"))
			if err != nil {
				t.Fatalf("%s as an admin: %v", tc.what, err)
			}
			if len(spendCarried(t, admin)) == 0 {
				t.Fatalf("%s carries no spend even to an admin, so this case certifies "+
					"nothing about withholding it", tc.what)
			}
			member, err := registryOver(t, tc.sources()).Answer(t.Context(), tc.what, tc.params,
				asMember("ops-1"))
			switch {
			case errors.Is(err, queries.ErrForbidden):
				// REFUSED rather than answered without it, which is the
				// one other honest answer — see the case below.
			case err != nil:
				t.Fatalf("%s as a member: %v", tc.what, err)
			default:
				if got := spendCarried(t, member); len(got) != 0 {
					t.Errorf("%s answers a member key what was spent, at %v", tc.what, got)
				}
			}
		})
	}
}

// A MEMBER ASKING A BOARD FOR COST BY NAME IS REFUSED, BY THE TERM TO DROP —
// never answered without it. The row fact promises a value when asked, and an
// order or a threshold on cost is the cost by another name; refused, the
// caller is told which term reaches past its key, as 403 `forbidden`. The
// same question without the terms is answered, and an admin is answered with
// them.
func TestAMemberAskingABoardForCostIsRefusedByTheTerm(t *testing.T) {
	t.Parallel()
	sources := partySources(t, nil)
	sources.Work = newSpendingWork()
	for _, params := range []map[string]any{
		{"fields": "tags,spend"},
		{"sort": "-spend_tokens"},
		{"spend_tokens": "gt:1000"},
		{"totals": "spend_turns:sum"},
		{"any": `[{"spend_tokens":"gt:1"},{"assignee":"ana"}]`},
	} {
		params["container"] = "workspace"
		_, err := askAs(t, sources, "work_items", params, asMember("ops-1"))
		if !errors.Is(err, queries.ErrForbidden) {
			t.Errorf("a member asking %v was answered %v, want forbidden", params, err)
			continue
		}
		if !strings.Contains(err.Error(), "spend") {
			t.Errorf("the refusal of %v does not name the term to drop: %v", params, err)
		}
		if _, err := askAs(t, sources, "work_items", params, asAdmin("ops-1")); err != nil {
			t.Errorf("an admin asking %v was refused: %v", params, err)
		}
	}
	if _, err := askAs(t, sources, "work_items", map[string]any{
		"container": "workspace", "fields": "tags,open_asks", "sort": "-updated",
	}, asMember("ops-1")); err != nil {
		t.Errorf("a member's board that reads no cost was refused: %v", err)
	}
}

// A DISABLED GUARD'S CALLER READS SPEND, as it reads every admin question: it
// is an admin's reach, and withholding from it would be a second idea of who
// a disabled guard lets in.
func TestADisabledGuardsCallerReadsSpend(t *testing.T) {
	t.Parallel()
	sources := partySources(t, nil)
	sources.Work = newSpendingWork()
	got, err := askAs(t, sources, "work_item", map[string]any{"id": "ENG-1"},
		auth.Principal{ID: auth.AnonymousOperator, Reach: auth.ReachAdmin})
	if err != nil {
		t.Fatalf("work_item: %v", err)
	}
	if len(spendCarried(t, got)) == 0 {
		t.Error("a disabled guard's caller was withheld what a task cost")
	}
}
