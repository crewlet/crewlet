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
// A node with no company answers every question and tool over the company's own
// halves `no_active_revision` with ONE sentence ([httpjson.NativeHalvesNotUp]),
// and each half reaches it through the same arm — `nativeAbsent(started,
// half)` in native.go, the half named as a word. So the sentence is the answer
// for every half that arm is ever called with, and one it does not name is
// told about the others: it named the tracker and the knowledge base while
// chat answered it too, and a Chatlet verb on such a node was told two halves it
// never asked for were not running. Read from the source, so a fourth half is
// held the day its first call lands. Mutation: drop chat from the sentence, and
// this fails.
func TestTheNoCompanySentenceNamesEveryHalfItAnswersFor(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "native.go", nil, 0)
	if err != nil {
		t.Fatalf("parse native.go: %v", err)
	}
	halves := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "nativeAbsent" {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("nativeAbsent is called naming its half %T, not as a word this "+
				"gate can hold against the sentence", call.Args[1])
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
}
