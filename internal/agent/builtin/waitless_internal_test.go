package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tools"
)

// EVERY UNAVAILABLE REFUSAL SAYS WHETHER A WAIT CLEARS IT, AND ONE NO WAIT
// CLEARS CARRIES NO WAIT.
//
// An unavailable refusal is answered by every surface under this node's
// undecided hint unless its cause says otherwise ([RetryAfter]), so a tool
// that told a person "skill synthesis is not configured on this deployment" or
// "this company does not run Chatlet" told them in the same answer to come
// back in two seconds: nothing about a deployment's configuration, a company's
// choice of backend or a surface built without a seam changes by waiting. So
// no unavailable refusal is a bare [refused]: each states its cause, [waits],
// [waitless] or what it met, and this walk refuses a bare one. It used to
// judge a bare one by its wording, matching "not configured" and the rest,
// which a sentence reworded in this package's own voice ("isn't configured")
// passed. The wording is still read, but only to catch a refusal marked
// [waits] that says what no wait clears, in either voice. Mutation: put
// `refused(tools.RefusalUnavailable, ...)` back on any of them, or mark
// memory.go's "not configured" or chat.go's "does not run Chatlet"
// [waits], and this fails naming its line.
func TestEveryUnavailableRefusalSaysWhetherAWaitClearsIt(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	// What a sentence says when nothing but a person's change clears it, in
	// both voices this package has written it in.
	phrases := []string{"not configured", "isn't configured", "does not run",
		"doesn't run", "built without", "wasn't built", "runs no ",
		"cannot declare", "can't declare", "could not be rendered",
		"couldn't be rendered"}
	read := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) //nolint:gosec // this package's own files
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || (fn.Name != "refused" && fn.Name != "refusedBy") {
				return true
			}
			if sel, ok := call.Args[0].(*ast.SelectorExpr); !ok || sel.Sel.Name != "RefusalUnavailable" {
				return true
			}
			read++
			if fn.Name == "refused" {
				t.Errorf("%s: an unavailable refusal says nothing about whether a "+
					"wait clears it, so every surface answers it under this node's "+
					"undecided wait: it is refusedBy(..., waits() or waitless(), ...)",
					fset.Position(call.Pos()))
				return true
			}
			if len(call.Args) != 3 {
				return true
			}
			mark, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := mark.Fun.(*ast.Ident); !ok || id.Name != "waits" {
				return true
			}
			said := literalText(call.Args[2])
			for _, phrase := range phrases {
				if strings.Contains(said, phrase) {
					t.Errorf("%s: an unavailable refusal saying %q is marked as one a "+
						"wait clears: no wait clears it, so it is waitless()",
						fset.Position(call.Pos()), phrase)
				}
			}
			return true
		})
	}
	// A WALK THAT READ NOTHING PASSES: the package has dozens of unavailable
	// refusals, and a parse that stopped finding calls would judge none.
	if read < 20 {
		t.Fatalf("read %d unavailable refusals: the walk is not finding them", read)
	}

	// AND THE MARK IS WHAT TAKES THE WAIT AWAY, where the other keeps it.
	marked := refusedBy(tools.RefusalUnavailable, waitless(), "Skill synthesis is not configured.")
	if RetryAfter(marked.Cause, time.Second) != 0 {
		t.Error("a waitless refusal still carries a wait")
	}
	waiting := refusedBy(tools.RefusalUnavailable, waits(), "The index is still being built.")
	if RetryAfter(waiting.Cause, time.Second) != time.Second {
		t.Error("a refusal a wait clears carries no wait")
	}
}

// literalText is every string literal in e, joined: what a sentence built from
// constants says, which is all the walk above needs to judge.
func literalText(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				b.WriteString(s)
			}
		}
		return true
	})
	return b.String()
}
