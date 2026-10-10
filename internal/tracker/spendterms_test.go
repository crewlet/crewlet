package tracker_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY WAY A QUESTION READS WHAT A TASK COST IS NAMED, so a surface that
// withholds cost (ADR-0031) can refuse it by the term to drop rather than
// answer a member with an order or a threshold that IS the cost. A term this
// misses is a board a member sorts, filters or totals by spend through.
func TestEveryWayAQuestionReadsCostIsNamed(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	parse := func(t *testing.T, params map[string]any) tracker.Query {
		t.Helper()
		params["container"] = "workspace"
		q, err := tracker.ParseQuery(tracker.MapParams(params), now, time.UTC)
		if err != nil {
			t.Fatalf("parse %v: %v", params, err)
		}
		return q
	}
	cases := []struct {
		name   string
		params map[string]any
		want   []string
	}{
		{"the row fact", map[string]any{"fields": "tags,spend"}, []string{"fields=spend"}},
		{"an order", map[string]any{"sort": "-spend_tokens,title"}, []string{"sort=spend_tokens"}},
		{"a threshold", map[string]any{"spend_tokens": "gt:1000"}, []string{"spend_tokens"}},
		{"a threshold in a branch", map[string]any{
			"any": `[{"assignee":"ana"},{"spend_tokens":"gt:5"}]`,
		}, []string{"any[1].spend_tokens"}},
		{"all of them, in order", map[string]any{
			"fields": "spend", "sort": "spend_tokens", "spend_tokens": "gt:0",
			"totals": "points:sum,spend_tokens:median",
		}, []string{"fields=spend", "sort=spend_tokens", "spend_tokens", "totals=spend_tokens:median"}},
	}
	// AND A TOTAL OVER EVERY COUNTER the applier derives, by the list the
	// struct, the DDL and the applier share — so a counter added there is
	// caught here without a case of its own.
	for _, column := range tracker.SpendColumns {
		if !strings.HasPrefix(column, tracker.SpendPrefix) {
			t.Errorf("the spend column %q is not named under %q, so a question "+
				"reading it is not seen to read cost", column, tracker.SpendPrefix)
		}
		cases = append(cases, struct {
			name   string
			params map[string]any
			want   []string
		}{"a total over " + column, map[string]any{"totals": column + ":sum"},
			[]string{"totals=" + column + ":sum"}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parse(t, tc.params).SpendTerms(); !slices.Equal(got, tc.want) {
				t.Errorf("%v reads cost through %v, want %v", tc.params, got, tc.want)
			}
		})
	}

	// THE CONTROL: a question that reads every OTHER opt-in fact, order,
	// threshold and total names nothing — or this would refuse every board.
	t.Run("a question about the work names nothing", func(t *testing.T) {
		t.Parallel()
		q := parse(t, map[string]any{
			"fields": "tags,dependents_count,open_asks", "sort": "-updated,estimate",
			"estimate": "gt:30", "points": "lt:8", "totals": "points:sum,estimate_min:avg,reopens:max",
			"any": `[{"assignee":"ana"},{"points":"gt:1"}]`,
		})
		if got := q.SpendTerms(); len(got) != 0 {
			t.Errorf("a question about the work is said to read cost through %v", got)
		}
	})
}
