package types

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// A BUDGET REFUSAL CARRIES THE WINDOW IT REFUSED IN, AND ONE FROM A BUILD THAT
// COUNTED NO WINDOW STILL DECODES.
//
// The window is what makes the record actionable — a day that resets tonight
// and a month that resets in three weeks are different decisions about raising
// a ceiling — so it has to survive the envelope both ways. And a rolling
// upgrade puts records from the lifetime-counter build on the same stream: one
// of those must decode with the window absent rather than failing, and must
// re-encode without inventing empty keys a reader would take for a window with
// no name.
func TestABudgetRefusalRoundTripsWithAndWithoutItsWindow(t *testing.T) {
	t.Parallel()
	refused := BudgetExhausted{
		Agent: "a-1", RoleName: "Lead", TurnID: "t-1", WorkKey: "wk-1",
		BudgetType: BudgetScopeOrg, UsedTokens: 99_000, MaxTokens: 100_000,
		Period: "day", Window: "2026-09-23", ResetsAt: "2026-09-24T07:00:00Z",
	}
	raw, err := json.Marshal(refused)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back BudgetExhausted
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != refused {
		t.Fatalf("round trip = %+v, want %+v", back, refused)
	}

	older := []byte(`{"agent_id":"a-1","role":"Lead","budget_type":"agent",` +
		`"used_tokens":10,"max_tokens":10}`)
	var fromOlder BudgetExhausted
	if err := json.Unmarshal(older, &fromOlder); err != nil {
		t.Fatalf("a record with no window did not decode: %v", err)
	}
	if fromOlder.Period != "" || fromOlder.Window != "" || fromOlder.ResetsAt != "" {
		t.Fatalf("a record with no window decoded one: %+v", fromOlder)
	}
	again, err := json.Marshal(fromOlder)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	for _, key := range []string{`"period"`, `"window"`, `"resets_at"`} {
		if strings.Contains(string(again), key) {
			t.Errorf("re-encoding a record with no window wrote %s: %s", key, again)
		}
	}
}

// THE DASHBOARD KNOWS EXACTLY THE BUDGET STATES THE ENGINE SENDS.
//
// The state is computed once, in the engine, precisely so no screen holds a
// threshold of its own — which makes the dashboard's `BudgetState` union the
// one place a screen can still disagree: a state it does not know renders as
// nothing, and one the engine never sends is a branch no window reaches. The
// engine's set is read off this package's own constants rather than listed
// here, so a fourth state added to the type and to nothing else fails.
func TestTheDashboardKnowsExactlyTheBudgetStatesTheEngineSends(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "budget.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse budget.go: %v", err)
	}
	var engine []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || value.Type == nil {
				continue
			}
			if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "BudgetState" {
				continue
			}
			for _, v := range value.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("a BudgetState constant is not a literal: %T", v)
				}
				state, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				if !BudgetState(state).Valid() {
					t.Errorf("BudgetState %q is declared and Valid refuses it", state)
				}
				engine = append(engine, state)
			}
		}
	}
	if len(engine) == 0 {
		t.Fatal("no BudgetState constants found, so this gate certifies nothing")
	}
	client, err := clientsource.Union("../"+clientsource.Tree, "BudgetState")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(engine)
	slices.Sort(client)
	if !slices.Equal(engine, client) {
		t.Errorf("the dashboard's BudgetState union is %v and the engine sends %v", client, engine)
	}
}

// A WINDOW STATES ONLY WHAT IS TRUE OF IT. An uncapped window carries no
// `limit` key — never 0, which reads as a range of nothing already full — and
// a window that has refused nothing carries no `refused_at`, since the
// dashboard tests that field for presence.
func TestABudgetWindowOmitsWhatItDoesNotHave(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(BudgetWindow{
		Period: "week", Window: "2026-W39", StartsAt: "2026-09-21T00:00:00Z",
		ResetsAt: "2026-09-28T00:00:00Z", Used: 12, State: BudgetOK,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, absent := range []string{`"limit"`, `"refused_at"`} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("an uncapped, unrefused window carries %s: %s", absent, raw)
		}
	}
	limit := 100
	raw, err = json.Marshal(BudgetWindow{Period: "day", Used: 100, Limit: &limit,
		RefusedAt: "2026-09-23T10:00:00Z", State: BudgetRefusing})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, present := range []string{`"limit":100`, `"refused_at":"2026-09-23T10:00:00Z"`, `"state":"refusing"`} {
		if !strings.Contains(string(raw), present) {
			t.Errorf("a refusing window lost %s: %s", present, raw)
		}
	}
}
