package tracker

import (
	"strings"
	"testing"
)

// TestTheAllOperatorCountsDistinctMembers is the arm that had no test at all,
// and a no-op sat inside it saying otherwise.
//
// `all` is a COUNT rather than an intersection — one subquery counting the
// distinct members a task holds from the named set, compared against the size
// of that set — and the shape that makes it one is a GROUP BY and a HAVING
// appended INSIDE the subquery's own parentheses. A `strings.Replace` of the
// select list with itself stood at the head of that arm, left over from a
// shape that widened the projection, and it read as though the count needed a
// different SELECT to work. Nothing exercised the arm, so nothing said
// otherwise.
func TestTheAllOperatorCountsDistinctMembers(t *testing.T) {
	t.Parallel()
	field := resolvedField{
		ID: "f-tags", Slug: "tags", Type: FieldLabels,
		Options: map[string]string{"api": "o-api", "ui": "o-ui"},
	}
	clause, args, err := fieldClause(
		FieldFilter{Op: FieldOpAll, Value: "api,ui"}, field)
	if err != nil {
		t.Fatalf("all: %v", err)
	}

	// THE GROUPING IS INSIDE THE SUBQUERY. Appended after its closing
	// paren the statement would not parse at all; appended to the OUTER
	// query it would group the task rows a board is drawing.
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

	// AND THE SET'S SIZE IS THE LAST ARGUMENT, or the HAVING compares
	// against whatever the previous placeholder bound.
	if len(args) == 0 {
		t.Fatal("no arguments")
	}
	if got := args[len(args)-1]; got != 2 {
		t.Fatalf("the count is compared against %v, want the set size 2 — "+
			"args %v", got, args)
	}
	// The option SPELLINGS are resolved to their ids, because that is
	// what a stored value holds.
	for _, want := range []any{"f-tags", "o-api", "o-ui", 2} {
		if !containsArg(args, want) {
			t.Fatalf("args %v do not carry %v", args, want)
		}
	}
}

// TestNotAllNegatesTheWholeMembership, which is the same distinction `not_any`
// carries one arm above: "this task does not hold all of these" is the
// negation of the COUNT, never a row-wise inequality — a task holding one of
// two named options satisfies a row-wise test for the other one and must not
// come back.
func TestNotAllNegatesTheWholeMembership(t *testing.T) {
	t.Parallel()
	field := resolvedField{
		ID: "f-tags", Slug: "tags", Type: FieldLabels,
		Options: map[string]string{"api": "o-api", "ui": "o-ui"},
	}
	all, _, err := fieldClause(
		FieldFilter{Op: FieldOpAll, Value: "api,ui"}, field)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	not, _, err := fieldClause(
		FieldFilter{Op: FieldOpNotAll, Value: "api,ui"}, field)
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
