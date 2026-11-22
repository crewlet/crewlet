package queries_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE RECENT AGENT CHANGES READ WHAT `growth_changes` SENDS: the answer, each
// row, its changed fields (a value, and published text sent as its digest
// alone) and its money, held both ways over the answer as the query writes it,
// with the steps and failures the log keeps.
//
// Mutation: drop `answerer` from GrowthChangeRow, or `after_bytes` from
// GrowthChangeField, and this is red.
func TestTheRecentAgentChangesReadWhatTheQuerySends(t *testing.T) {
	t.Parallel()
	log := &growthLog{rows: []accessdomain.GrowthRow{{Handle: "growth", Turn: "t-1", HeldCall: "h-1",
		Product: "fakeads", Connection: "eu", Account: "act_1", Target: "c1", Tool: "fakeads_raise_budget",
		Class: growth.ClassSpend, Step: growth.StepFailed, AnsweredBy: accessdomain.GrowthByPerson, Answerer: "Ada",
		Operation: "ab", Before: "cd", Failure: growth.FailureVendorRefused,
		Changes: []accessdomain.GrowthChange{
			{Field: "daily_budget", Label: "Daily budget", Before: "50000000", After: "60000000", Money: true},
			{Field: "reply", Label: "Reply", Text: true, BeforeSHA256: "ef", BeforeBytes: 3, AfterSHA256: "cd",
				AfterBytes: 12}},
		Money: &accessdomain.GrowthMoney{Currency: "EUR", DeltaMicros: 1, DailyMicros: 2, DailyAfterMicros: 3,
			PerChangeCapMicros: 4, DailyCapMicros: 5},
		At: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}}}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Growth: log, Chart: authz.NoChart{}})
	got, err := r.Answer(seated(t, "ada", iam.GrantAdmin), "growth_changes", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var answer map[string]any
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatal(err)
	}
	holdSent(t, "GrowthChangesAnswer", []map[string]any{answer})
	rows := objectsOf(t, answer["rows"])
	holdSent(t, "GrowthChangeRow", rows)
	holdSent(t, "GrowthChangeField", objectsOf(t, rows[0]["changes"]))
	holdSent(t, "GrowthChangeMoney", objectsOf(t, rows[0]["money"]))
	holdWords(t, "GrowthStep", growth.Steps(), false)
	holdWords(t, "GrowthFailure", growth.Failures(), true)
}

// holdSent holds an interface to answers both ways: each declared member is in
// some answer, and each key an answer sends is declared.
func holdSent(t *testing.T, iface string, rows []map[string]any) {
	t.Helper()
	members, err := clientsource.Interface(clientsource.Tree(t), iface)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	declared := map[string]bool{}
	for _, m := range members {
		declared[m.Name] = true
	}
	sent := map[string]bool{}
	for _, row := range rows {
		for key := range row {
			sent[key] = true
		}
	}
	for _, field := range slices.Sorted(maps.Keys(declared)) {
		if !sent[field] {
			t.Errorf("%s declares %s and no answer sends it", iface, field)
		}
	}
	for _, field := range slices.Sorted(maps.Keys(sent)) {
		if !declared[field] {
			t.Errorf("an answer sends %s.%s and %s does not declare it", iface, field, iface)
		}
	}
}

// holdWords holds a union to the engine's closed set, with "" where none is a value.
func holdWords[S ~string](t *testing.T, union string, want []S, empty bool) {
	t.Helper()
	got, err := clientsource.Union(clientsource.Tree(t), union)
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	words := []string{}
	if empty {
		words = append(words, "")
	}
	for _, w := range want {
		words = append(words, string(w))
	}
	if slices.Sort(got); !slices.Equal(got, slices.Sorted(slices.Values(words))) {
		t.Errorf("%s declares %v, the engine sends %v", union, got, words)
	}
}

// objectsOf is a JSON object or list of objects as rows.
func objectsOf(t *testing.T, v any) []map[string]any {
	t.Helper()
	switch v := v.(type) {
	case map[string]any:
		return []map[string]any{v}
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("a list item is %T, not an object", item)
			}
			out = append(out, m)
		}
		return out
	}
	t.Fatalf("%T is not an object", v)
	return nil
}
