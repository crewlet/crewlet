package builtin_test

import (
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY `outcome` A TOOL BUILDS IS A [statelog.Outcome], by type: the act route
// answers anything else `unknown`, telling a person a landed change may not have.
// Mutation: answer `"outcome": "unchanged"` from any tool, and it is reported.
func TestEveryOutcomeAToolAnswersIsAWriteOutcome(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	fset := token.NewFileSet()
	const statelogPath = "github.com/crewlet/crewlet/internal/statelog"
	isOutcome := func(typ types.Type) bool {
		n, ok := types.Unalias(typ).(*types.Named)
		return ok && n.Obj().Name() == "Outcome" && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == statelogPath
	}
	checked := 0
	err := sourcetree.TypeCheck{Fset: fset, Patterns: []string{"./internal/agent/builtin"}}.Run(root,
		func(u *sourcetree.Unit) error {
			// holds reports whether v is an Outcome, or a string conversion of one.
			holds := func(v ast.Expr) bool {
				if call, ok := v.(*ast.CallExpr); ok && len(call.Args) == 1 {
					if tv, ok := u.Info.Types[call.Fun]; ok && tv.IsType() {
						return isOutcome(u.Info.TypeOf(call.Args[0]))
					}
				}
				return isOutcome(u.Info.TypeOf(v))
			}
			judge := func(at token.Pos, v ast.Expr) {
				checked++
				if !holds(v) {
					t.Errorf("%s: `outcome` holds a %s, not a statelog.Outcome: the act route answers it unknown",
						fset.Position(at), u.Info.TypeOf(v))
				}
			}
			for _, file := range u.Files {
				ast.Inspect(file, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.KeyValueExpr:
						if isOutcomeKey(n.Key) {
							judge(n.Pos(), n.Value)
						}
					case *ast.AssignStmt:
						for i, lhs := range n.Lhs {
							if ix, ok := lhs.(*ast.IndexExpr); ok && isOutcomeKey(ix.Index) && len(n.Rhs) == len(n.Lhs) {
								judge(n.Pos(), n.Rhs[i])
							}
						}
					case *ast.Field:
						if n.Tag != nil && jsonName(n.Tag.Value) == "outcome" {
							checked++
							if !isOutcome(u.Info.TypeOf(n.Type)) {
								t.Errorf("%s: field `outcome` is a %s, not a statelog.Outcome: the act route "+
									"answers it unknown", fset.Position(n.Pos()), u.Info.TypeOf(n.Type))
							}
						}
					}
					return true
				})
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// THE WALK READ THE TOOLS: dozens of write tools answer an outcome.
	if checked < 30 {
		t.Fatalf("the walk judged %d outcomes, want every write tool's", checked)
	}
}

// isOutcomeKey reports whether e is the string literal "outcome".
func isOutcomeKey(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && s == "outcome"
}

// jsonName is the JSON name a struct tag literal gives its field.
func jsonName(tag string) string {
	raw, err := strconv.Unquote(tag)
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(reflect.StructTag(raw).Get("json"), ",")
	return name
}

// EVERY WRITE A PERSON CAN MAKE ANSWERS A WRITE OUTCOME OR NONE, over a landed
// answer of each, so an `outcome` built at run time is held too.
// Mutation: answer `"outcome": "created"` from any write, and it is reported.
func TestEveryWriteAPersonCanMakeAnswersAWriteOutcome(t *testing.T) {
	t.Parallel()
	for name, res := range everyLandedAnswer(t) {
		out := personAnswer(t, name, res)
		said, stated := out["outcome"]
		if !stated {
			continue
		}
		if s, ok := said.(string); !ok || !statelog.Outcome(s).Valid() {
			t.Errorf("%s answers outcome %v, which the act route reads as unknown: want one of %v",
				name, said, statelog.Outcomes)
		}
	}
}
