package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// THE NO-COMPANY SENTENCE NAMES EVERY HALF IT IS ANSWERED FOR.
//
// A node with no company answers every question and tool over the company's
// own halves `no_active_revision` with ONE sentence
// ([httpjson.NativeHalvesNotUp]), each half reaching it through `nativeAbsent(
// engine, started, half)` in native.go. So the sentence must name every half
// that arm is called with: one it omits is told about the others (a Chatlet
// verb was told the tracker and knowledge base were not running). Read from the
// source, so a fourth half is held the day its first call lands. And IN A
// PERSON'S WORDS, not the engine's account of its bring-up ("has not been
// handed a company", "come up with its first revision"). Mutation: drop chat
// from the sentence, or put either phrase back, and this fails.
func TestTheNoCompanySentenceNamesEveryHalfItAnswersFor(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "native.go", nil, 0)
	if err != nil {
		t.Fatalf("parse native.go: %v", err)
	}
	halves := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "nativeAbsent" {
			return true
		}
		lit, ok := call.Args[2].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("nativeAbsent is called naming its half %T, not as a word this "+
				"gate can hold against the sentence", call.Args[2])
			return true
		}
		half, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatalf("unquote %s: %v", lit.Value, err)
		}
		halves[half] = true
		return true
	})
	if len(halves) == 0 {
		t.Fatal("native.go calls nativeAbsent nowhere, so this gate reads nothing")
	}
	for half := range halves {
		if !strings.Contains(httpjson.NativeHalvesNotUp, half) {
			t.Errorf("the %s answers a node with no company %q, which never names "+
				"it", half, httpjson.NativeHalvesNotUp)
		}
	}
	for _, engines := range []string{"handed", "revision"} {
		if strings.Contains(httpjson.NativeHalvesNotUp, engines) {
			t.Errorf("a person reads %q, which says %q", httpjson.NativeHalvesNotUp, engines)
		}
	}
}
