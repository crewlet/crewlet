package worklet

import (
	"regexp"
	"strings"
	"testing"
)

// A PREDICATE COMPILED FOR AN ALIAS NAMES NO OTHER.
//
// One predicate is asked of two different tasks ([alias]): of the row a
// statement returns, and of the task that row's subtree hangs from, which a
// subquery reads under a name of its own. A clause that wrote `t.` where it
// should have written its alias would not fail anywhere — inside the subquery
// `t` is still the OUTER row, so the clause would quietly ask the row's own
// question of the row instead of its anchor's, and the answer would be another
// set with nothing in it to say so. So every query the grammar can compile —
// the gate's registered set in both subtask modes, and a case for every clause
// the registered set does not reach — is compiled for an alias other than the
// row's, and must not name the row's alias anywhere.
//
// NOT VACUOUS: each compiled predicate must name the alias it was handed, so a
// compiler that ignored the alias and wrote none would fail here too.
func TestAPredicateCompiledForAnAliasNamesNoOther(t *testing.T) {
	t.Parallel()
	row := regexp.MustCompile(`\b` + string(rowAlias) + `\.`)
	cases := registeredQueries()
	for name, params := range inBothModes(everyClause()) {
		cases[name] = params
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			q := planQuery(t, params)
			if q.PriorityListOf != "" {
				// RESOLVED BY THE READER inside its transaction
				// ([readPriorityList]); a list stands in for it here.
				q.PriorityList = []string{"t-00001", "t-00002"}
			}
			where, _, err := compilePredicate(q, planNow, planFields(q), false,
				anchorAlias)
			if err != nil {
				t.Fatalf("compilePredicate: %v", err)
			}
			predicate := strings.Join(where, " AND ")
			if loc := row.FindStringIndex(predicate); loc != nil {
				t.Errorf("the predicate compiled for %q names %q at %q, which "+
					"inside the rollup's subquery is the OUTER row:\n%s",
					anchorAlias, rowAlias, predicate[loc[0]:min(loc[1]+20,
						len(predicate))], predicate)
			}
			if !strings.Contains(predicate, string(anchorAlias)+".") {
				t.Errorf("the predicate compiled for %q never names it:\n%s",
					anchorAlias, predicate)
			}
		})
	}
}

// everyClause is a query for every clause [compilePredicate] writes that the
// plan gate's registered set ([registeredQueries]) does not reach, so a test
// over both is a test of the whole compiler.
func everyClause() map[string]map[string]any {
	return map[string]map[string]any{
		"a reporter or nobody":       {"reporter": "h-1,none"},
		"unassigned":                 {"assignee": "none"},
		"a reference":                {"references": "ENG-1"},
		"with a parent":              {"has_parent": "true"},
		"without a parent":           {"has_parent": "false"},
		"with an open ask":           {"has_open_asks": "true"},
		"without an open ask":        {"has_open_asks": "false"},
		"asked of somebody":          {"asked_of": "h-1"},
		"asked by somebody":          {"asked_by": "h-1"},
		"a find":                     {"q": "login"},
		"not a status":               {"status": "!done,todo"},
		"a priority":                 {"priority": "high"},
		"a priority list":            {"priorities": "h-1"},
		"every task flag":            {"flag": "cycle,too_deep,inconsistent_project,key_collision"},
		"archived only":              {"archived": "only"},
		"not blocked":                {"blocked": "false"},
		"blocking nothing":           {"blocking": "false"},
		"no dependencies":            {"has_dependencies": "false"},
		"a field not equal":          {"f.owner": "ne:x"},
		"a field holding not all":    {"f.areas": "not_all:a,b"},
		"a field holding none of":    {"f.areas": "not_any:a,b"},
		"a field holding all":        {"f.areas": "all:a,b"},
		"a field in a range":         {"f.effort": "range:1..5"},
		"a field containing":         {"f.owner": "contains:x"},
		"every date column":          {"created": "gte:2030-01-01", "updated": "range:2030-01-01..2031-01-01", "status_entered": "lt:today", "done": "lt:today", "closed": "lt:today", "finished": "lt:today", "archived_at": "lt:today"},
		"closed since a day":         {"closed_since": "sow"},
		"spend":                      {"spend_tokens": "gt:1"},
		"no estimate":                {"estimate": "null"},
		"two branches":               {"any": `[{"assignee":"h-1","tag":"none:a"},{"assignee":"none","container":"project:P01","due":"lt:today","f.owner":"x"}]`},
		"a column of a status board": {"container": "project:P01", "group_by": "status", "group": "todo"},
	}
}
