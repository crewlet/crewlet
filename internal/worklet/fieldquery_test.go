package worklet

import (
	"strings"
	"testing"
)

// TestTheAllOperatorCountsDistinctMembers: `all` is a count of the distinct
// members a task holds from the set, compared with the set's size, by a
// GROUP BY and HAVING inside the subquery's own parentheses.
func TestTheAllOperatorCountsDistinctMembers(t *testing.T) {
	t.Parallel()
	field := resolvedField{
		ID: "f-tags", Slug: "tags", Type: FieldLabels,
		Options: map[string]string{"api": "o-api", "ui": "o-ui"},
	}
	clause, args, err := fieldClause(
		FieldFilter{Op: FieldOpAll, Value: "api,ui"}, field, rowAlias)
	if err != nil {
		t.Fatalf("all: %v", err)
	}

	// The grouping is inside the subquery: on the outer query it would
	// group the task rows a board draws.
	if !strings.HasSuffix(clause, ")") {
		t.Fatalf("the subquery is not closed: %s", clause)
	}
	for _, want := range []string{
		"GROUP BY v.task_id",
		"HAVING COUNT(DISTINCT v.ref) = ?",
	} {
		if !strings.Contains(clause, want) {
			t.Fatalf("the clause does not count distinct members — %q is "+
				"missing from:\n%s", want, clause)
		}
	}
	if strings.Index(clause, "GROUP BY") > strings.LastIndex(clause, ")") {
		t.Fatalf("the grouping is outside the subquery: %s", clause)
	}

	// The set's size is the last argument, the HAVING's placeholder.
	if len(args) == 0 {
		t.Fatal("no arguments")
	}
	if got := args[len(args)-1]; got != 2 {
		t.Fatalf("the count is compared against %v, want the set size 2 — "+
			"args %v", got, args)
	}
	// Option spellings resolve to the ids a stored value holds.
	for _, want := range []any{"f-tags", "o-api", "o-ui", 2} {
		if !containsArg(args, want) {
			t.Fatalf("args %v do not carry %v", args, want)
		}
	}
}

// TestNotAllNegatesTheWholeMembership: `not_all` negates the count, never a
// row-wise inequality, which a task holding one of two options would meet.
func TestNotAllNegatesTheWholeMembership(t *testing.T) {
	t.Parallel()
	field := resolvedField{
		ID: "f-tags", Slug: "tags", Type: FieldLabels,
		Options: map[string]string{"api": "o-api", "ui": "o-ui"},
	}
	all, _, err := fieldClause(
		FieldFilter{Op: FieldOpAll, Value: "api,ui"}, field, rowAlias)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	not, _, err := fieldClause(
		FieldFilter{Op: FieldOpNotAll, Value: "api,ui"}, field, rowAlias)
	if err != nil {
		t.Fatalf("not_all: %v", err)
	}
	if not == all {
		t.Fatal("not_all compiled to the same clause as all")
	}
	if !strings.Contains(not, "NOT IN") && !strings.Contains(not, "NOT (") {
		t.Fatalf("not_all does not negate the membership: %s", not)
	}
	if !strings.Contains(not, "GROUP BY v.task_id") {
		t.Fatalf("not_all lost the count it is the negation of: %s", not)
	}
}

func containsArg(args []any, want any) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
