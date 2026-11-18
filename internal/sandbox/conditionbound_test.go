package sandbox

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// thisPackagePath is this package's import path, which a [Condition] constant of
// another package names its type through.
const thisPackagePath = "github.com/crewlet/crewlet/internal/sandbox"

// EVERY CONDITION THE MODULE NAMES FITS WHAT AN OWED REFUSAL RECORDS. A
// refusal the control topic acknowledges is recorded on the job's record by
// its condition ([PendingStore.OweRefusal]), and the record takes a condition
// no longer than [MaxOwedConditionBytes], the share of the row the field is
// given. One past it is refused as a validation error, which the coordinator
// hands back as the store's silence: every delivery refused on that condition
// would be handed back, never acknowledged nor recorded, for as long as the
// condition held, and its warning would say the store did not take the record.
// So every constant of type [Condition], in this package and in every package
// that names one (the engine's gate conditions above all), is held to the
// bound here, where the constant is written, rather than found by the store at
// the first refusal. The walk type-checks each package, so a constant whose
// type is implied by its group, or spelled through an alias, is read too; and
// it must find the engine's conditions as well as this package's, or it read
// nothing of the module beyond this package.
//
// Mutation: name a condition past the bound, and its constant is reported.
func TestEveryConditionFitsWhatAnOwedRefusalRecords(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	fset := token.NewFileSet()
	type named struct {
		pos   token.Position
		name  string
		value string
	}
	var found []named
	err := sourcetree.TypeCheck{
		Fset: fset,
		Select: func(p *sourcetree.Listed) bool {
			return p.ImportPath == thisPackagePath || importsThisPackage(t, p)
		},
	}.Run(root, func(u *sourcetree.Unit) error {
		for ident, obj := range u.Info.Defs {
			c, ok := obj.(*types.Const)
			if !ok || !isConditionType(c.Type()) || c.Val().Kind() != constant.String {
				continue
			}
			found = append(found, named{pos: fset.Position(ident.Pos()), name: c.Name(),
				value: constant.StringVal(c.Val())})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, len(found))
	for _, c := range found {
		values = append(values, c.value)
		if len(c.value) > MaxOwedConditionBytes {
			t.Errorf("%s: condition %s is %q, %d bytes, past the %d an owed refusal records "+
				"(MaxOwedConditionBytes): every delivery refused on it would be handed back unrecorded",
				c.pos, c.name, c.value, len(c.value), MaxOwedConditionBytes)
		}
	}
	// THE WALK READ THE MODULE: this package's conditions and the engine's.
	for _, want := range []string{string(SeatLost), string(Draining), "ownership", "provider"} {
		if !slices.Contains(values, want) {
			t.Errorf("the walk found no condition %q among %d (%v); want every package that names one read",
				want, len(values), values)
		}
	}
}

// isConditionType reports whether typ is this package's [Condition].
func isConditionType(typ types.Type) bool {
	n, ok := types.Unalias(typ).(*types.Named)
	return ok && n.Obj().Name() == "Condition" && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == thisPackagePath
}

// importsThisPackage reports whether one of p's non-test files imports this
// package, the only way a package outside it names a [Condition] constant.
func importsThisPackage(t *testing.T, p *sourcetree.Listed) bool {
	t.Helper()
	for _, name := range p.GoFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(p.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			// THE CHECK REPORTS WHAT IT CANNOT PARSE: selected, its parse
			// fails the walk.
			return true
		}
		if slices.ContainsFunc(f.Imports, func(spec *ast.ImportSpec) bool {
			path, unquoteErr := strconv.Unquote(spec.Path.Value)
			return unquoteErr == nil && path == thisPackagePath
		}) {
			return true
		}
	}
	return false
}
