package queries

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE TURNS THAT CHANGED NOTHING, PER AGENT AND WAKE: every node's started turns
// summed by what woke them, a held wake never counted as a turn, the spend of
// those that committed nothing apart (the cache inside the input, counted once),
// and each agent named by the chart's handle.
func TestTurnOutcomesSumStartedTurnsByAgentAndWake(t *testing.T) {
	t.Parallel()
	rows := []usage.TurnLedgerRow{
		{AgentID: "a-1", Handle: "old", Wake: "schedule:nightly", Disposition: "started", Outcome: "nothing",
			Turns: 3, Input: 100, Output: 20},
		{AgentID: "a-1", Wake: "schedule:nightly", Disposition: "started", Outcome: "nothing",
			Turns: 2, Input: 50, CacheRead: 30},
		{AgentID: "a-1", Wake: "schedule:nightly", Disposition: "started", Outcome: "moved",
			Turns: 1, Input: 40, AuxOutput: 5},
		{AgentID: "a-1", Wake: "schedule:nightly", Disposition: "held", Outcome: "", Turns: 9},
		{AgentID: "a-2", Wake: "work_item:pickup", Disposition: "started", Outcome: "words", Turns: 4, Output: 8},
	}
	chart := map[string]chartSeat{"a-1": {handle: "ops", agentID: "a-1"}}
	got := foldTurnOutcomes(rows, chart, "")
	want := []TurnOutcomeRow{
		{Seat: "a-2", AgentID: "a-2", Wake: "work_item:pickup", Turns: 4, Tokens: 8},
		{Seat: "ops", AgentID: "a-1", Wake: "schedule:nightly", Turns: 6, Tokens: 215,
			Nothing: 5, NothingTokens: 170},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("folded\n%+v\nwant\n%+v", got, want)
	}
	if only := foldTurnOutcomes(rows, chart, "a-2"); len(only) != 1 || only[0].AgentID != "a-2" {
		t.Errorf("a window narrowed to a-2 gave %+v", only)
	}
}

// THE DASHBOARD SAYS EVERY WAKE KIND THE ENGINE KEYS: contract/spend.ts's
// WakeKind is exactly the `types.Origin*` constants, read from their source so a
// new kind fails here until the Spend page has words for it.
func TestTheDashboardSaysEveryWakeKind(t *testing.T) {
	t.Parallel()
	client, err := clientsource.Union(clientsource.Tree(t), "WakeKind")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(sourcetree.Root(t), "internal/events/types/types.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var engine []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			v := spec.(*ast.ValueSpec)
			for i, name := range v.Names {
				lit, ok := v.Values[i].(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && strings.HasPrefix(name.Name, "Origin") {
					engine = append(engine, strings.Trim(lit.Value, `"`))
				}
			}
		}
	}
	slices.Sort(engine)
	slices.Sort(client)
	if len(engine) == 0 || !slices.Equal(client, engine) {
		t.Errorf("the dashboard's wake kinds are %v, the engine's %v", client, engine)
	}
}
