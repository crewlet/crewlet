package storetest

import (
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// A STATEMENT SPELLED ACROSS LITERALS IS ONE FRAGMENT, AND A CALL INSIDE A
// CONCATENATION IS READ TOO.
//
// [StatementLiterals] holds a gate's registered statements complete by finding
// every fragment of the source that reads a table, and the fragment is the
// unit it matches whole. So a statement written across adjacent literals must
// fold into the one string it spells — matched literal by literal, `"… FROM "`
// and `"pages_heads p"` each name nothing — while a non-literal operand ends
// the run, and a statement passed to a call inside a concatenation is a
// fragment of its own.
//
// Mutation: end a run at every literal rather than at a non-literal operand,
// and the folded statement comes back in pieces; stop visiting the operands of
// a concatenation, and the call's statement is lost.
func TestAStatementSpelledAcrossLiteralsIsOneFragment(t *testing.T) {
	t.Parallel()
	const source = `package p

func f() {
	_ = "SELECT p.id FROM " + "pages_heads p" + " WHERE " + clause + " ORDER BY p.id"
	_ = "x = " + read("SELECT 1 FROM " + "pages_labels") + "y"
}
`
	file, err := parser.ParseFile(token.NewFileSet(), "p.go", source, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := literalRuns(file)
	want := []string{
		"SELECT p.id FROM pages_heads p WHERE ", " ORDER BY p.id",
		"x = ", "SELECT 1 FROM pages_labels", "y",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the fragments are %q, want %q", got, want)
	}
}
