package tracker_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// rowFieldsCompany is two tasks with one of everything a card can ask about:
// `a` carries two labels, waits on `b`, holds an open question and has cost a
// turn; `b` carries nothing but the dependent.
func rowFieldsCompany(t *testing.T) *roundTrip {
	t.Helper()
	r := newRoundTrip(t)
	r.declareTags("api", "ui")
	a := newTask("a")
	a.Key, a.Tags = "ENG-a", []string{"ui", "api"}
	if _, err := r.writer.CreateTask(t.Context(), "op-a", a, nil); err != nil {
		t.Fatalf("CreateTask a: %v", err)
	}
	r.drain()
	filedTask(t, r, "b")
	if _, err := r.writer.Depend(t.Context(), "op-depend", tracker.DependencyChange{
		Task: "a", Project: "ENG", WaitingOnAdd: []string{"b"},
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	r.drain()
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", "a", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: "a", Author: "ana", AuthorKind: tracker.AuthorHuman,
			Body: "which endpoint?", Ask: "bob", CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "turn/run-1/dispatch",
		sampleTurn("a")); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	r.drain()
	return r
}

// rowOf is one task's row in an answer, flat or grouped.
func rowOf(t *testing.T, answer tracker.Answer, id string) tracker.TaskRow {
	t.Helper()
	rows := slices.Clone(answer.Rows)
	for _, group := range answer.Groups {
		rows = append(rows, group.Rows...)
	}
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no row for %s in the answer", id)
	return tracker.TaskRow{}
}

// A BOARD CARD'S FACTS ARE ASKED FOR, AND PRESENT EVEN AT ZERO WHEN THEY ARE.
//
// The row a seat reads through `list_work_items` is the row every board reads
// too, and every byte on it is in a model's context on every listing — so a
// card's labels, its "blocks N", its open question and its cost ride only on
// an answer that named them. And a card that asked is told "0" and "[]" rather
// than left with an absent key: "asked, and none" and "the engine did not
// understand the question" must not look the same.
func TestRowFieldsAreOptIn(t *testing.T) {
	t.Parallel()
	r := rowFieldsCompany(t)

	plain := r.ask(map[string]any{"container": "project:ENG"})
	for _, id := range []string{"a", "b"} {
		row := rowOf(t, plain, id)
		if row.Tags != nil || row.DependentsCount != nil || row.OpenAsks != nil ||
			row.Spend != nil {
			t.Errorf("%s carries row facts nobody asked for: %+v", id, row)
		}
	}
	body, err := json.Marshal(plain.Rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"tags"`, `"dependents_count"`, `"open_asks"`,
		`"spend"`} {
		if strings.Contains(string(body), key) {
			t.Errorf("an answer that asked for no row facts carries %s on the "+
				"wire: %s", key, body)
		}
	}

	asked := map[string]any{
		"container": "project:ENG",
		"fields":    "tags,dependents_count,open_asks,spend",
	}
	for name, params := range map[string]map[string]any{
		"flat":  asked,
		"board": withKey(asked, "group_by", "status"),
	} {
		t.Run(name, func(t *testing.T) {
			answer := r.ask(params)
			a, b := rowOf(t, answer, "a"), rowOf(t, answer, "b")
			if !slices.Equal(a.Tags, []string{"api", "ui"}) {
				t.Errorf("a's labels are %v, want [api ui], sorted", a.Tags)
			}
			if b.Tags == nil || len(b.Tags) != 0 {
				t.Errorf("b's labels are %#v, want an empty list — asked and "+
					"none is not absent", b.Tags)
			}
			if b.DependentsCount == nil || *b.DependentsCount != 1 {
				t.Errorf("b blocks %v, want 1: a waits on it", b.DependentsCount)
			}
			if a.DependentsCount == nil || *a.DependentsCount != 0 {
				t.Errorf("a blocks %v, want 0 — present, because it was asked",
					a.DependentsCount)
			}
			if a.OpenAsks == nil || *a.OpenAsks != 1 {
				t.Errorf("a holds %v open questions, want the one asked of bob",
					a.OpenAsks)
			}
			want := tracker.RowSpend{Tokens: 1400, Turns: 1, Workers: 2, SentBack: 1}
			if a.Spend == nil || *a.Spend != want {
				t.Errorf("a cost %+v, want its own turn's %+v", a.Spend, want)
			}
			if b.Spend == nil || *b.Spend != (tracker.RowSpend{}) {
				t.Errorf("b cost %+v, want a present zero — it has run no turn",
					b.Spend)
			}
			body, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{`"tags":[]`, `"dependents_count":1`,
				`"open_asks":0`, `"spend":{"tokens":0`} {
				if !strings.Contains(string(body), key) {
					t.Errorf("b's row on the wire lacks %s: %s", key, body)
				}
			}
		})
	}

	// AN UNKNOWN FACT IS REFUSED BY NAME, never ignored: a card that asked
	// for `spnd` would otherwise draw every task as having cost nothing.
	if _, err := tracker.ParseQuery(tracker.MapParams{
		"container": "project:ENG", "fields": "spnd",
	}, wednesday, berlin); err == nil || !strings.Contains(err.Error(), "spnd") {
		t.Errorf("fields=spnd parsed (err %v), want a refusal naming it", err)
	}
}

// THE MOST EXPENSIVE TASKS ARE A SORT, and the key names the column.
//
// `sort=-spend_tokens` is the Spend screen's "most expensive tasks": the order
// has to be the tokens the rows then report, or the list ranks by one number
// and draws another.
func TestAListSortsBySpendTokensAndCarriesThem(t *testing.T) {
	t.Parallel()
	r := rowFieldsCompany(t)
	filedTask(t, r, "c")
	if _, err := r.writer.RecordTurn(t.Context(), "turn/run-2/dispatch",
		tracker.TurnRecord{
			Task: "c", Seat: "dev", TurnID: "run-2", Trigger: "work_item",
			Outcome: "done", Phases: []string{"execute"},
			Spend: tracker.TurnSpend{Turns: 1, Input: 9000, Output: 1000},
		}); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	r.drain()

	answer := r.ask(map[string]any{
		"container": "project:ENG", "sort": "-spend_tokens", "fields": "spend",
	})
	if got := ids(answer); !slices.Equal(got, []string{"c", "a", "b"}) {
		t.Fatalf("sort=-spend_tokens answers %v, want [c a b] — ten thousand, "+
			"fourteen hundred, nothing", got)
	}
	var tokens []int
	for _, row := range answer.Rows {
		tokens = append(tokens, row.Spend.Tokens)
	}
	if !slices.IsSortedFunc(tokens, func(x, y int) int { return y - x }) {
		t.Errorf("the rows report %v tokens in the order the sort put them — "+
			"the list ranks by one number and draws another", tokens)
	}
	// AND THE OLD SPELLING IS GONE rather than aliased: `spend` stopped
	// naming one number the day a row could carry five of them.
	if _, err := tracker.ParseQuery(tracker.MapParams{
		"container": "project:ENG", "sort": "-spend",
	}, wednesday, berlin); err == nil {
		t.Error("sort=-spend still parses")
	}
}

// withKey is a copy of params with one more key.
func withKey(params map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out[key] = value
	return out
}
